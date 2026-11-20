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
	readRecord := func(t *testing.T) *domain.IdempotencyRecord {
		t.Helper()
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.RestoreEntryOperation, key)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}

	// 2. 确认同键同参可复用，且再次请求会保留首次恢复快照。
	committed := readRecord(t)
	digest, err := service.BuildRestoreEntryRequestHash(entryID, revision)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := service.ValidateRestoreEntryReplay(committed, actor.UserID, entryID, revision, key, digest, now); err != nil {
		t.Fatal("刚提交的恢复记录无法复用", err)
	}
	otherDigest, err := service.BuildRestoreEntryRequestHash(entryID, revision+1)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.ValidateRestoreEntryReplay(committed, actor.UserID, entryID, revision+1, key, otherDigest, now); !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
		t.Fatal("同键不同预期版本未报冲突", err)
	}
	if err := service.ValidateRestoreEntryReplay(committed, actor.UserID, entryID, revision+1, key, otherDigest, committed.ExpiresAt); !errors.Is(err, domain.ErrIdempotencyRecordExpired) {
		t.Fatal("到达保留截止时间时应先拒绝复用", err)
	}
	originalBody := committed.ResponseBody
	decoded, err := dto.DecodeRestoreEntryData(originalBody)
	if err != nil || decoded == nil || *decoded != *expected || decoded.Entry.ParentID != "11" || decoded.Entry.Revision != "6" {
		t.Fatal("持久 JSON 未恢复为预期快照", decoded, err)
	}

	// 3. 核对记录关联与完整快照各自拦截的不一致情况。
	for _, scenario := range []struct {
		name, statement        string
		badReplay, badSnapshot bool
	}{
		{"numeric revision", "UPDATE idempotency_records SET response_body=JSON_SET(?, '$.entry.revision', 6) WHERE user_id=? AND operation=? AND idempotency_key=?", true, true},
		{"wrong status", "UPDATE idempotency_records SET response_body=JSON_SET(?, '$.entry.status', 'TRASHED') WHERE user_id=? AND operation=? AND idempotency_key=?", false, true},
		{"missing parent", "UPDATE idempotency_records SET response_body=JSON_REMOVE(?, '$.entry.parent_id') WHERE user_id=? AND operation=? AND idempotency_key=?", false, true},
		{"valid snapshot for another entry", "UPDATE idempotency_records SET response_body=JSON_SET(?, '$.entry.id', '16') WHERE user_id=? AND operation=? AND idempotency_key=?", true, false},
		{"valid snapshot with wrong version", "UPDATE idempotency_records SET response_body=JSON_SET(?, '$.entry.revision', '7') WHERE user_id=? AND operation=? AND idempotency_key=?", true, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			update, err := pool.ExecContext(ctx, scenario.statement, string(originalBody), actor.UserID, domain.RestoreEntryOperation, key)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := update.RowsAffected(); err != nil || count != 1 {
				t.Fatal("未更新独占库的快照", count, err)
			}
			record := readRecord(t)
			replayError := service.ValidateRestoreEntryReplay(record, actor.UserID, entryID, revision, key, digest, now)
			if scenario.badReplay && !errors.Is(replayError, domain.ErrInvalidIdempotencyRecordData) || !scenario.badReplay && replayError != nil {
				t.Fatal("持久记录与请求的关联判断不符", replayError)
			}
			data, decodeError := dto.DecodeRestoreEntryData(record.ResponseBody)
			if scenario.badSnapshot && (data != nil || !errors.Is(decodeError, domain.ErrInvalidIdempotencyRecordData)) ||
				!scenario.badSnapshot && (data == nil || decodeError != nil) {
				t.Fatal("持久快照的解码结果不符", data, decodeError)
			}
		})
	}
}
