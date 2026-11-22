package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestIdempotentCreateUploadReplacementServiceIntegration(t *testing.T) {
	// 1. 在独占库安装正式会话与幂等表，准备两个账号和同账号的两个设备。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	receiptStart := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if sessionStart < 0 || sessionEnd <= sessionStart || receiptStart < 0 {
		t.Fatal("正式事务夹具表定义未找到")
	}
	execute := func(t *testing.T, sql string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	for _, sql := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd], migrations.InitialSQL[receiptStart:],
		"UPDATE file_entries SET revision=6 WHERE id=31",
		"UPDATE users SET used_storage=16777216,reserved_storage=512 WHERE id=7",
		"UPDATE users SET status='ACTIVE' WHERE id=8",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES (15,7,'28ee758a-0727-42de-ae7d-3bb33155e159','second fixture device','WEB')",
		`INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES
			(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),
			(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),
			(19,8,12,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
	} {
		execute(t, sql)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	second := domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
	other := domain.Actor{UserID: 8, DeviceID: 12, SessionID: 19}
	const singleLimit = domain.MaxSingleUploadSizeBytes
	limits := domain.UploadPlanLimits{MaxFileSizeBytes: 2 * singleLimit, MaxObjectSizeBytes: 2 * singleLimit, MaxPartSizeBytes: singleLimit}
	base := service.UploadReplacementDeclaration{EntryID: 31, ExpectedRevision: 6, SizeBytes: 40, ExpectedSHA256: sha256.Sum256([]byte("synthetic content"))}
	newCreator := func(saveError error, encode func(*domain.UploadTask) (json.RawMessage, error)) *service.IdempotentCreateUploadReplacementService {
		return service.NewIdempotentCreateUploadReplacementService("MINIO", "fixture-bucket",
			func(callContext context.Context, current domain.Actor, action func(context.Context, service.IdempotentCreateUploadReplacementTransaction) error) error {
				return store.WithUserTransaction(callContext, current, func(txContext context.Context, txStore *Store) error {
					if saveError != nil {
						return action(txContext, &idempotentUploadSaveFailureStore{Store: txStore, failure: saveError})
					}
					return action(txContext, txStore)
				})
			}, encode, dto.ValidateCreateUploadReplacementSnapshot)
	}
	creator := newCreator(nil, dto.EncodeCreateUploadReplacementSnapshot)

	type writes struct{ used, reserve, taskReserve, objects, tasks, receipts, sequence uint64 }
	readState := func(t *testing.T, userID uint64) writes {
		t.Helper()
		var state writes
		err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,change_seq,
			(SELECT COALESCE(SUM(reserved_bytes),0) FROM upload_tasks WHERE user_id=users.id AND replacement_entry_id IS NOT NULL),
			(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=users.id),
			(SELECT COUNT(*) FROM upload_tasks WHERE user_id=users.id),
			(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id)
			FROM users WHERE id=?`, userID).
			Scan(&state.used, &state.reserve, &state.sequence, &state.taskReserve, &state.objects, &state.tasks, &state.receipts)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	expectOne := func(t *testing.T, before writes, size uint64) {
		t.Helper()
		want := before
		want.reserve += size
		want.taskReserve += size
		want.objects++
		want.tasks++
		want.receipts++
		if after := readState(t, actor.UserID); after != want || after.reserve != after.taskReserve+512 {
			t.Fatalf("四项提交或已有预占不符，得到 %+v，期望 %+v", after, want)
		}
	}
	checkReceipt := func(t *testing.T, key string, declaration service.UploadReplacementDeclaration, body []byte, earliest time.Time) {
		t.Helper()
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateUploadReplacementOperation, key)
		hash, hashError := service.BuildCreateUploadReplacementRequestHash(declaration.EntryID, declaration.ExpectedRevision, declaration.SizeBytes, declaration.ExpectedSHA256)
		if err != nil || hashError != nil || record.ResourceID == nil || record.RequestHash != hash || record.ResponseStatus != 201 ||
			record.ResourceType != "UPLOAD_TASK" || !bytes.Equal(record.ResponseBody, body) ||
			record.ExpiresAt.Before(earliest.Add(8*24*time.Hour)) || record.ExpiresAt.After(time.Now().Add(8*24*time.Hour)) {
			t.Fatalf("已保存回执的任务、范围或保留期不符 %+v，错误 %v", record, err)
		}
		if err := dto.ValidateCreateUploadReplacementSnapshot(body, *record.ResourceID, declaration.EntryID, declaration.ExpectedRevision, declaration.SizeBytes); err != nil {
			t.Fatal(err)
		}
		task, err := store.FindUserUploadTask(ctx, actor.UserID, *record.ResourceID)
		if err != nil || task.ReservedBytes != declaration.SizeBytes || task.ExpectedSHA256 == nil || *task.ExpectedSHA256 != declaration.ExpectedSHA256 {
			t.Fatalf("已提交回执关联的任务不符 %+v，错误 %v", task, err)
		}
	}

	// 2. 三种大小均验证首次提交、保存回执和同账号另一设备的重试。
	for _, size := range []uint64{0, singleLimit, singleLimit + 1} {
		key := "size-" + strconv.FormatUint(size, 10)
		t.Run(key, func(t *testing.T) {
			input := base
			input.SizeBytes = size
			before := readState(t, actor.UserID)
			earliest := time.Now().UTC().Truncate(time.Microsecond)
			first, err := creator.Create(ctx, actor, input, key, limits)
			if err != nil {
				t.Fatal(err)
			}
			checkReceipt(t, key, input, first, earliest)
			again, err := creator.Create(ctx, second, input, key, domain.UploadPlanLimits{})
			if err != nil || !bytes.Equal(again, first) {
				t.Fatalf("换设备或限额配置后未保留原回执，错误 %v", err)
			}
			expectOne(t, before, size)
		})
	}

	t.Run("concurrent devices share one receipt", func(t *testing.T) {
		before := readState(t, actor.UserID)
		type result struct {
			body []byte
			err  error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for _, current := range []domain.Actor{actor, second} {
			go func() {
				<-start
				body, err := creator.Create(ctx, current, base, "same-concurrent-key", limits)
				results <- result{body: body, err: err}
			}()
		}
		close(start)
		first, secondResult := <-results, <-results
		if first.err != nil || secondResult.err != nil || len(first.body) == 0 || !bytes.Equal(first.body, secondResult.body) {
			t.Fatalf("同键并发未取得同一回执，错误 %v 和 %v", first.err, secondResult.err)
		}
		expectOne(t, before, base.SizeBytes)
	})

	// 3. 核对同键变参、过期及身份变化的拒绝路径。
	t.Run("conflicts expiry and current authorization", func(t *testing.T) {
		const key = "same-concurrent-key"
		before, otherBefore := readState(t, actor.UserID), readState(t, other.UserID)
		reject := func(current domain.Actor, input service.UploadReplacementDeclaration, want error) {
			t.Helper()
			body, err := creator.Create(ctx, current, input, key, limits)
			if body != nil || !errors.Is(err, want) || readState(t, actor.UserID) != before || readState(t, other.UserID) != otherBefore {
				t.Fatalf("拒绝保留的状态不符，预期 %v，错误 %v", want, err)
			}
		}
		changed := base
		changed.ExpectedSHA256[0] ^= 1
		reject(actor, changed, domain.ErrIdempotencyRequestConflict)
		reject(other, base, domain.ErrFileEntryNotFound)
		execute(t, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=18")
		reject(second, base, domain.ErrSessionRevoked)
		execute(t, "UPDATE file_entries SET revision=7 WHERE id=31")
		if body, err := creator.Create(ctx, actor, base, key, limits); err != nil || len(body) == 0 {
			t.Fatalf("目标文件升版后历史回执无法复用，错误 %v", err)
		}
		execute(t, "UPDATE file_entries SET revision=6 WHERE id=31")
		execute(t, "UPDATE idempotency_records SET created_at=UTC_TIMESTAMP(6)-INTERVAL 9 DAY,expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE user_id=7 AND idempotency_key='same-concurrent-key'")
		reject(actor, changed, domain.ErrIdempotencyRecordExpired)
	})

	// 4. 注入编码失败和保存后失败，确认四项一起回滚，随后同键仍可首次创建。
	t.Run("receipt failure rolls back all writes", func(t *testing.T) {
		const key = "rollback-receipt"
		fault := errors.New("test replacement receipt write failed")
		before := readState(t, actor.UserID)
		for _, failed := range []*service.IdempotentCreateUploadReplacementService{
			newCreator(nil, func(*domain.UploadTask) (json.RawMessage, error) { return nil, fault }),
			newCreator(fault, dto.EncodeCreateUploadReplacementSnapshot),
		} {
			body, err := failed.Create(ctx, actor, base, key, limits)
			if body != nil || !errors.Is(err, fault) || readState(t, actor.UserID) != before {
				t.Fatalf("回执故障留下部分写入，错误 %v", err)
			}
		}
		if _, err := creator.Create(ctx, actor, base, key, limits); err != nil {
			t.Fatal(err)
		}
		expectOne(t, before, base.SizeBytes)
	})
}
