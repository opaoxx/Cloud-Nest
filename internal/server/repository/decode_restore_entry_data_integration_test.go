package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestDecodeRestoreEntryDataIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000' WHERE user_id=1 AND id=15"); err != nil {
		t.Fatal(err)
	}
	const key = "restore-snapshot-001"
	entryID := uint64(15)
	revision := uint64(5)

	// 1. 在独占库保存实际恢复结果，覆盖回退根目录的父目录标识。
	var expected *dto.RestoreEntryData
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		core := service.NewRestoreEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, service.RestoreEntryTransaction) error) error {
			return action(ctx, txStore)
		})
		result, err := core.Restore(txContext, actor, entryID, revision)
		if err != nil {
			return err
		}
		expected, err = dto.NewRestoreEntryData(result)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(expected)
		if err != nil {
			return err
		}
		digest, err := service.BuildRestoreEntryRequestHash(entryID, revision)
		if err != nil {
			return err
		}
		_, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
			Operation: domain.RestoreEntryOperation, IdempotencyKey: key, RequestHash: digest,
			ResourceType: "ENTRY", ResourceID: &entryID, ResponseStatus: 200, ResponseBody: encoded,
			ExpiresAt: time.Now().Add(8 * 24 * time.Hour),
		})
		return err
	})
	if err != nil {
		t.Fatal("建立持久恢复快照失败", err)
	}
	readSnapshot := func(t *testing.T) []byte {
		t.Helper()
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.RestoreEntryOperation, key)
		if err != nil {
			t.Fatal(err)
		}
		return record.ResponseBody
	}

	// 2. 从真实 MySQL JSON 列读回快照，核对实际位置和版本。
	originalBody := readSnapshot(t)
	decoded, err := dto.DecodeRestoreEntryData(originalBody)
	if err != nil || decoded == nil || *decoded != *expected || decoded.Entry.ParentID != "11" || decoded.Entry.Revision != "6" {
		t.Fatal("持久 JSON 未恢复为预期快照", decoded, err)
	}

	// 3. MySQL 可保存不符合业务契约的 JSON，验证解码器逐项拒绝。
	for _, scenario := range []struct {
		name, statement string
	}{
		{"numeric revision", "UPDATE idempotency_records SET response_body=JSON_SET(?, '$.entry.revision', 6) WHERE user_id=? AND operation=? AND idempotency_key=?"},
		{"wrong status", "UPDATE idempotency_records SET response_body=JSON_SET(?, '$.entry.status', 'TRASHED') WHERE user_id=? AND operation=? AND idempotency_key=?"},
		{"missing parent", "UPDATE idempotency_records SET response_body=JSON_REMOVE(?, '$.entry.parent_id') WHERE user_id=? AND operation=? AND idempotency_key=?"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			update, err := pool.ExecContext(ctx, scenario.statement, string(originalBody), actor.UserID, domain.RestoreEntryOperation, key)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := update.RowsAffected(); err != nil || count != 1 {
				t.Fatal("未更新独占库的快照", count, err)
			}
			data, err := dto.DecodeRestoreEntryData(readSnapshot(t))
			if data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常数据库快照被解码为成功结果", data, err)
			}
		})
	}
}
