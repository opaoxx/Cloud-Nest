package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestPersistSyncSnapshotCreationIntegration(t *testing.T) {
	// 1. 使用独占测试库的正式表与已有活动设备。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	_, syncDevicesDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	idempotencyStart := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if !found || idempotencyStart < 0 {
		t.Fatal("未找到同步设备或幂等回执表")
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(syncDevicesDDL)
	execute(migrations.InitialSQL[idempotencyStart:])
	execute("UPDATE users SET change_seq=35 WHERE id=?", actor.UserID)
	oldGeneration := domain.SyncCursorGeneration{0, 0xfe, 2, 0xff}
	execute("INSERT INTO sync_devices(device_id,user_id,ack_sequence,status,cursor_generation,last_seen_at) "+
		"VALUES(?,?,35,'ACTIVE',?,UTC_TIMESTAMP(6))", actor.DeviceID, actor.UserID, oldGeneration[:])

	binding := service.SyncSnapshotCacheBinding{
		UserID: actor.UserID, DeviceID: actor.DeviceID, PageSize: 50,
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"),
		Generation: oldGeneration,
	}
	binding.Generation[15] = 0x80
	now := time.Now().In(time.FixedZone("fixture", 8*60*60))
	const key = "SyncSnapshot-Create-001"
	hash, err := service.BuildCreateSyncSnapshotRequestHash(actor.DeviceID, binding.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(receiptKey string, requestHash [sha256.Size]byte, checkedAt time.Time) (*domain.SyncSnapshotReceiptLocator, error) {
		var locator *domain.SyncSnapshotReceiptLocator
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var lookupError error
			locator, lookupError = service.FindCreateSyncSnapshotReplayInTransaction(
				txContext, actor, receiptKey, requestHash, checkedAt, txStore.FindUserIdempotencyRecord, dto.DecodeCreateSyncSnapshotReceipt,
			)
			return lookupError
		})
		return locator, err
	}
	type deviceState struct {
		ack           uint64
		status        string
		generation    string
		seen, updated time.Time
	}
	readDeviceState := func() deviceState {
		t.Helper()
		var state deviceState
		err := pool.QueryRowContext(ctx, "SELECT ack_sequence,status,HEX(cursor_generation),last_seen_at,updated_at FROM sync_devices WHERE device_id=?",
			actor.DeviceID).Scan(&state.ack, &state.status, &state.generation, &state.seen, &state.updated)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := readDeviceState()
	persist := func(cutoff uint64, afterWrite func(context.Context, *Store, error)) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			locator, err := service.FindCreateSyncSnapshotReplayInTransaction(
				txContext, actor, key, hash, now, txStore.FindUserIdempotencyRecord, dto.DecodeCreateSyncSnapshotReceipt,
			)
			if err != nil || locator != nil {
				t.Fatalf("首次写入前查找应为空：%+v，%v", locator, err)
			}
			err = service.PersistSyncSnapshotCreationInTransaction(txContext, actor, binding, cutoff, key, now, txStore,
				dto.EncodeCreateSyncSnapshotReceipt, dto.DecodeCreateSyncSnapshotReceipt)
			if afterWrite != nil {
				afterWrite(txContext, txStore, err)
			}
			return err
		})
	}
	assertUnchanged := func(t *testing.T) {
		t.Helper()
		if actual := readDeviceState(); actual != before {
			t.Fatalf("设备状态未保留：%+v，期望 %+v", actual, before)
		}
		if locator, err := lookup(key, hash, now); locator != nil || err != nil {
			t.Fatalf("失败后出现回执或查询异常：%+v，错误 %v", locator, err)
		}
	}

	// 2. 截止序号失效时不写入，真实回执 INSERT 失败时回滚已执行的重置。
	if err := persist(34, nil); !errors.Is(err, domain.ErrSyncSnapshotExpired) {
		t.Fatal("空日志不能衔接此前的快照截止序号", err)
	}
	assertUnchanged(t)
	execute("CREATE TRIGGER fixture_reject_snapshot_receipt BEFORE INSERT ON idempotency_records FOR EACH ROW " +
		"SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture receipt insert rejected'")
	err = persist(35, func(txContext context.Context, txStore *Store, writeError error) {
		var databaseError *mysql.MySQLError
		if !errors.As(writeError, &databaseError) || databaseError.Number != 1644 {
			t.Fatalf("未经过预期的 MySQL 回执写入错误：%v", writeError)
		}
		var ackSequence uint64
		var status string
		var generation []byte
		queryError := txStore.database.WithContext(txContext).Raw(
			"SELECT ack_sequence,status,cursor_generation FROM sync_devices WHERE device_id=?", actor.DeviceID,
		).Row().Scan(&ackSequence, &status, &generation)
		if queryError != nil || ackSequence != 0 || status != "RESET_REQUIRED" || len(generation) != len(binding.Generation) ||
			domain.SyncCursorGeneration(generation) != binding.Generation {
			t.Fatalf("回执失败前未经过真实的基线重置：确认位置=%d，状态=%s，错误=%v", ackSequence, status, queryError)
		}
	})
	if err == nil {
		t.Fatal("回执写入被拒绝却提交了事务")
	}
	assertUnchanged(t)

	// 3. 取消故障夹具，验证相同输入可提交两项写入并解码回执。
	execute("DROP TRIGGER fixture_reject_snapshot_receipt")
	if err := persist(35, nil); err != nil {
		t.Fatal("创建提交失败", err)
	}
	committed := readDeviceState()
	if committed.status != "RESET_REQUIRED" || committed.ack != 0 {
		t.Fatalf("已提交的设备状态为 %+v", committed)
	}
	generation, err := store.FindSyncDeviceGeneration(ctx, actor.UserID, actor.DeviceID)
	if err != nil || generation != binding.Generation {
		t.Fatalf("设备已提交代次为 %x，错误 %v", generation, err)
	}
	record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateSyncSnapshotOperation, key)
	if err != nil || record == nil {
		t.Fatal("未读到已提交的创建回执", err)
	}
	locator, err := lookup(key, hash, now)
	if err != nil || locator == nil || locator.SnapshotID != binding.SnapshotID || locator.Generation != generation ||
		record.ResourceID != nil || record.ResponseStatus != http.StatusCreated ||
		!record.ExpiresAt.Equal(now.UTC().Truncate(time.Microsecond).Add(8*24*time.Hour)) {
		t.Fatalf("已保存回执或原快照定位不符：%+v，%+v，错误 %v", record, locator, err)
	}
	otherHash, err := service.BuildCreateSyncSnapshotRequestHash(actor.DeviceID, binding.PageSize+1)
	if err != nil {
		t.Fatal(err)
	}
	if locator, err := lookup(strings.ToLower(key), hash, now); locator != nil || err != nil {
		t.Fatalf("大小写不同的键错误命中：%+v，%v", locator, err)
	}
	if locator, err := lookup(key, otherHash, now); locator != nil || !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
		t.Fatalf("同键异参结果为 %+v，错误 %v", locator, err)
	}
	if locator, err := lookup(key, hash, record.ExpiresAt); locator != nil || !errors.Is(err, domain.ErrIdempotencyRecordExpired) {
		t.Fatalf("到期回执结果为 %+v，错误 %v", locator, err)
	}
	if actual := readDeviceState(); actual != committed {
		t.Fatal("查找回执改变了同步设备的状态")
	}

	// 4. 单连接条件下，先释放查回执的事务，再读取已提交代次和原页。
	clock := func() time.Time { return now }
	cache := service.NewSyncSnapshotCache(clock)
	reservation, err := cache.ReserveBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt, err := cache.Put(ctx, reservation, binding, &service.BuiltSyncSnapshot{
		Header:  domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 35},
		Entries: domain.SyncSnapshotEntriesJSON{Items: []json.RawMessage{}, SizeBytes: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	signingKey := bytes.Repeat([]byte{0x65}, sha256.Size)
	cursorSigner, err := service.NewSyncSnapshotCursorSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	baselineSigner, err := service.NewSyncSnapshotBaselineSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	pageReader := service.NewSyncSnapshotReplayService(store.FindSyncDeviceGeneration, cache.Get,
		service.NewSyncSnapshotPageDelivery(cursorSigner.Sign, baselineSigner.Sign))
	replay := service.NewSyncSnapshotCreationReplay(
		func(ctx context.Context, identity domain.Actor, action func(context.Context, service.SyncSnapshotReceiptReader) error) error {
			return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
				return action(txContext, txStore)
			})
		}, dto.DecodeCreateSyncSnapshotReceipt, pageReader, clock)
	page, err := replay.ReadExistingFirstPage(ctx, actor, key, binding.PageSize)
	if err != nil || page == nil || page.SnapshotID != binding.SnapshotID || page.Header.HighWaterSequence != 35 ||
		page.ExpiresAt != expiresAt || page.BaselineToken == nil || page.NextCursor != nil {
		t.Fatalf("单连接下同键重读返回 %+v，错误 %v", page, err)
	}
	if page, err := replay.ReadExistingFirstPage(ctx, actor, "Fresh-Snapshot-Request", binding.PageSize); page != nil || err != nil {
		t.Fatalf("首次创建判断返回 %+v，错误 %v", page, err)
	}
}
