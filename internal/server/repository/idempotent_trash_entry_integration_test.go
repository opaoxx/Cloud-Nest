package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestIdempotentTrashEntryServiceIntegration(t *testing.T) {
	// 1. 复用独占库及正式表，装配真实认证事务与 DTO 编解码。
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	withTransaction := func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentTrashEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	}
	encode := func(entry *domain.TrashedFileEntry) (json.RawMessage, error) {
		data, err := dto.NewTrashEntryData(entry)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}
	validate := func(body json.RawMessage) error {
		_, err := dto.RestoreTrashEntryData(body)
		return err
	}
	trash := service.NewIdempotentTrashEntryService(withTransaction, encode, validate)
	read := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	readRecord := func(t *testing.T, key string) *domain.IdempotencyRecord {
		t.Helper()
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.TrashEntryOperation, key)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	expectError := func(t *testing.T, entryID, revision uint64, key string, want error) {
		t.Helper()
		body, err := trash.Trash(ctx, actor, entryID, revision, key)
		if body != nil || !errors.Is(err, want) {
			t.Fatal("幂等失败语义不符", err)
		}
	}

	// 2. 首次提交回收与快照。同键重试时即使文件已恢复且改名，仍返回历史结果。
	t.Run("first commit and historical replay", func(t *testing.T) {
		const key = "trash-first"
		first, err := trash.Trash(ctx, actor, 13, 3, key)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := dto.RestoreTrashEntryData(first)
		record := readRecord(t, key)
		entry := read(t, 13)
		if err != nil || snapshot == nil || snapshot.Entry.ID != "13" || snapshot.Entry.Revision != "4" ||
			snapshot.Entry.Status != "TRASHED" || entry.Status != "TRASHED" || entry.Revision != 4 ||
			!entry.ModifiedAt.Equal(snapshot.Entry.DeletedAt) || !bytes.Equal(first, record.ResponseBody) ||
			record.ExpiresAt.Sub(record.CreatedAt) < 7*24*time.Hour {
			t.Fatal("首次回收和稳定结果未一并提交", snapshot, err)
		}
		expectError(t, 13, 4, key, domain.ErrIdempotencyRequestConflict)
		expectError(t, 13, 4, "trash-new-key", domain.ErrFileEntryNotFound)

		// 直接布置后续状态，模拟该项已经从回收站恢复并改名。
		if _, err := pool.ExecContext(ctx, `UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,revision=5,
			name='后来文件.md',name_key=CAST('后来文件.md' AS BINARY) WHERE user_id=1 AND id=13`); err != nil {
			t.Fatal(err)
		}
		before := read(t, 13)
		replay, err := trash.Trash(ctx, actor, 13, 3, key)
		if err != nil || !bytes.Equal(first, replay) || !reflect.DeepEqual(before, read(t, 13)) {
			t.Fatal("同键重试未保留原快照或改动了后续状态", err)
		}
		if _, err := pool.ExecContext(ctx, `UPDATE idempotency_records
			SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY,created_at=UTC_TIMESTAMP(6)-INTERVAL 2 DAY
			WHERE user_id=? AND operation=? AND idempotency_key=?`, actor.UserID, domain.TrashEntryOperation, key); err != nil {
			t.Fatal(err)
		}
		expectError(t, 13, 3, key, domain.ErrIdempotencyRecordExpired)
	})

	// 3. 事务回调写入后返回失败时，状态、时间和幂等记录一起回滚。
	t.Run("outer failure after saving rolls back both writes", func(t *testing.T) {
		const key = "trash-rollback"
		before := read(t, 12)
		fixtureError := errors.New("fixture transaction fails after saving")
		failing := service.NewIdempotentTrashEntryService(
			func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentTrashEntryTransaction) error) error {
				return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
					if err := action(txContext, txStore); err != nil {
						return err
					}
					return fixtureError
				})
			}, encode, validate,
		)
		body, err := failing.Trash(ctx, actor, 12, 1, key)
		record, readError := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.TrashEntryOperation, key)
		var deletedAt sql.NullTime
		timeError := pool.QueryRowContext(ctx, "SELECT deleted_at FROM file_entries WHERE user_id=1 AND id=12").Scan(&deletedAt)
		if body != nil || !errors.Is(err, fixtureError) || record != nil || !errors.Is(readError, domain.ErrIdempotencyRecordNotFound) ||
			timeError != nil || deletedAt.Valid || !reflect.DeepEqual(read(t, 12), before) {
			t.Fatal("外层失败后未同时回滚目录项和幂等记录", err, readError, timeError)
		}
	})

	// 4. 多连接并发同键同参，用户锁串行化首次回收。
	t.Run("concurrent same key returns one committed snapshot", func(t *testing.T) {
		const key = "trash-concurrent"
		pool.SetMaxOpenConns(3)
		defer pool.SetMaxOpenConns(1)
		type outcome struct {
			body json.RawMessage
			err  error
		}
		const callers = 4
		results := make(chan outcome, callers)
		start := make(chan struct{})
		for range callers {
			go func() {
				<-start
				body, err := trash.Trash(ctx, actor, 12, 1, key)
				results <- outcome{body: body, err: err}
			}()
		}
		close(start)
		observed := make([]outcome, 0, callers)
		for range callers {
			observed = append(observed, <-results)
		}
		committed := observed[0].body
		for _, result := range observed {
			if result.err != nil || len(result.body) == 0 || !bytes.Equal(result.body, committed) {
				t.Fatal("同键并发未返回相同快照", result.err)
			}
		}
		record := readRecord(t, key)
		entry := read(t, 12)
		if !bytes.Equal(record.ResponseBody, committed) || entry.Status != "TRASHED" || entry.Revision != 2 {
			t.Fatal("同键并发的保存快照或回收版本不符", entry)
		}
	})
}
