package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
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
			prepared := service.PreparedSyncSnapshot{
				Binding: binding, Header: domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: cutoff},
				ExpiresAt: now.Add(10 * time.Minute),
			}
			locator, err := service.FinalizeSyncSnapshotCreationInTransaction(txContext, actor, key, &prepared, now, txStore,
				dto.EncodeCreateSyncSnapshotReceipt, dto.DecodeCreateSyncSnapshotReceipt)
			if locator != nil {
				t.Fatalf("首次写入错误地复用了回执：%+v", locator)
			}
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

	// 5. 两条连接用同一个新键竞争，只有一个候选能写入设备和回执。
	t.Run("concurrent finalization with same key", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		const sharedKey = "Concurrent-Snapshot-Create"
		first := service.PreparedSyncSnapshot{
			Binding: binding, Header: domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 35},
			ExpiresAt: now.Add(10 * time.Minute),
		}
		first.Binding.SnapshotID[15] ^= 1
		first.Binding.Generation[15] ^= 1
		second := first
		second.Binding.SnapshotID[15] ^= 2
		second.Binding.Generation[15] ^= 2

		finalize := func(requestKey string, candidate service.PreparedSyncSnapshot) (*domain.SyncSnapshotReceiptLocator, error) {
			var existing *domain.SyncSnapshotReceiptLocator
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				var finalizeError error
				existing, finalizeError = service.FinalizeSyncSnapshotCreationInTransaction(
					txContext, actor, requestKey, &candidate, now, txStore,
					dto.EncodeCreateSyncSnapshotReceipt, dto.DecodeCreateSyncSnapshotReceipt,
				)
				return finalizeError
			})
			return existing, err
		}
		type outcome struct {
			binding  service.SyncSnapshotCacheBinding
			existing *domain.SyncSnapshotReceiptLocator
			err      error
		}
		start := make(chan struct{})
		results := make(chan outcome, 2)
		for _, candidate := range []service.PreparedSyncSnapshot{first, second} {
			go func() {
				<-start
				existing, err := finalize(sharedKey, candidate)
				results <- outcome{binding: candidate.Binding, existing: existing, err: err}
			}()
		}
		close(start)
		winner, follower := <-results, <-results
		if winner.existing != nil {
			winner, follower = follower, winner
		}
		if winner.err != nil || follower.err != nil || winner.existing != nil || follower.existing == nil ||
			follower.existing.SnapshotID != winner.binding.SnapshotID || follower.existing.Generation != winner.binding.Generation {
			t.Fatalf("并发结果未指向同一快照：写入 %+v，复用 %+v", winner, follower)
		}
		savedGeneration, err := store.FindSyncDeviceGeneration(ctx, actor.UserID, actor.DeviceID)
		if err != nil || savedGeneration != winner.binding.Generation {
			t.Fatalf("设备保存了错误代次 %x，错误 %v", savedGeneration, err)
		}
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records WHERE user_id=? AND operation=? AND idempotency_key=?",
			actor.UserID, domain.CreateSyncSnapshotOperation, sharedKey).Scan(&count); err != nil || count != 1 {
			t.Fatalf("并发同键回执数为 %d，错误 %v", count, err)
		}

		// 6. 到期的候选只要命中回执仍可复用；没有回执时不能写入。
		committed := readDeviceState()
		second.ExpiresAt = now
		if locator, err := finalize(sharedKey, second); err != nil || locator == nil || *locator != *follower.existing {
			t.Fatalf("候选到期时已有回执复用结果为 %+v，%v", locator, err)
		}
		if locator, err := finalize("Expired-Candidate", second); locator != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) {
			t.Fatalf("到期且无回执的候选返回 %+v，%v", locator, err)
		}
		second.Binding.PageSize++
		if locator, err := finalize(sharedKey, second); locator != nil || !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
			t.Fatalf("同键异参在最终事务中返回 %+v，%v", locator, err)
		}
		if readDeviceState() != committed {
			t.Fatal("复用或拒绝候选改变了设备状态")
		}
	})

	// 7. 两个创建服务请求都错过首查，由最后的账号事务选出同一份快照。
	t.Run("creation service reuses one committed page", func(t *testing.T) {
		requestContext, stop := context.WithCancel(ctx)
		defer stop()
		const createServiceKey = "Concurrent-Creation-Service"
		building, continueBuild := make(chan struct{}, 2), make(chan struct{})
		preparation := service.NewSyncSnapshotPreparation(func(ctx context.Context, _ domain.Actor) (*service.BuiltSyncSnapshot, error) {
			building <- struct{}{}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-continueBuild:
				return &service.BuiltSyncSnapshot{
					Header:  domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 35},
					Entries: domain.SyncSnapshotEntriesJSON{Items: []json.RawMessage{}, SizeBytes: 2},
				}, nil
			}
		}, service.NewSyncSnapshotCache(clock))
		creator := service.NewSyncSnapshotCreationService(
			preparation, func(ctx context.Context, identity domain.Actor, action func(context.Context, service.SyncSnapshotFinalizationTransaction) error) error {
				return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
					return action(txContext, txStore)
				})
			}, store.FindSyncDeviceGeneration, service.NewSyncSnapshotPageDelivery(cursorSigner.Sign, baselineSigner.Sign),
			dto.EncodeCreateSyncSnapshotReceipt, dto.DecodeCreateSyncSnapshotReceipt, clock,
		)
		type creationResult struct {
			page *domain.SyncSnapshotPageResult
			err  error
		}
		results := make(chan creationResult, 2)
		for range 2 {
			go func() {
				page, err := creator.Create(requestContext, actor, createServiceKey, binding.PageSize)
				results <- creationResult{page: page, err: err}
			}()
		}
		for range 2 {
			select {
			case <-requestContext.Done():
				t.Fatal("两个请求没有都进入构建", requestContext.Err())
			case <-building:
			case result := <-results:
				t.Fatal("账号首查未进入构建", result.err)
			}
		}
		close(continueBuild)
		first, second := <-results, <-results
		if first.err != nil || second.err != nil || first.page == nil || !reflect.DeepEqual(first.page, second.page) || first.page.BaselineToken == nil {
			t.Fatalf("同键并发首页不同：%+v，%+v", first, second)
		}
		retried, err := creator.Create(requestContext, actor, createServiceKey, binding.PageSize)
		if err != nil || !reflect.DeepEqual(first.page, retried) || len(building) != 0 {
			t.Fatalf("已提交回执未跳过构建或首页变化：%+v，%v", retried, err)
		}
	})
}
