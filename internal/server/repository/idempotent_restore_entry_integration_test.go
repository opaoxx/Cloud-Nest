package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type restoreSaveFailureStore struct {
	*Store
	failure error
}

func (store restoreSaveFailureStore) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	saved, err := store.Store.CreateUserIdempotencyRecord(ctx, input)
	if err != nil {
		return nil, err
	}
	return saved, store.failure
}

func TestIdempotentRestoreEntryServiceIntegration(t *testing.T) {
	// 1. 用正式表准备可原位恢复和需回退根目录的文件，接上真实 DTO。
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	for _, statement := range []string{
		"UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000' WHERE user_id=1 AND id IN (13,15)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(17,1,11,'隐藏.MD',CAST('隐藏.md' AS BINARY),'DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备幂等恢复夹具失败", err)
		}
	}
	withTransaction := func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentRestoreEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	}
	encode := func(entry *domain.FileEntry) (json.RawMessage, error) {
		result, err := dto.NewRestoreEntryData(entry)
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	}
	validate := func(body json.RawMessage) error {
		_, err := dto.DecodeRestoreEntryData(body)
		return err
	}
	restore := service.NewIdempotentRestoreEntryService(withTransaction, encode, validate)
	readEntry := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	readRecord := func(key string) (*domain.IdempotencyRecord, error) {
		return store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.RestoreEntryOperation, key)
	}

	// 2. 核对首次事务和幂等记录，再验证同键重试只交付历史快照。
	t.Run("first restore and historical replay", func(t *testing.T) {
		const key = "restore-first"
		beforeCall := time.Now().UTC().Truncate(time.Microsecond)
		first, err := restore.Restore(ctx, actor, 15, 5, key)
		if err != nil {
			t.Fatal(err)
		}
		afterCall := time.Now().UTC()
		data, err := dto.DecodeRestoreEntryData(first)
		entry := readEntry(t, 15)
		if err != nil || data == nil || entry.ParentID == nil || *entry.ParentID != 11 || entry.Status != "ACTIVE" ||
			entry.Name != "隐藏 (1).md" || entry.Revision != 6 || data.Entry.ParentID != "11" ||
			data.Entry.Name != entry.Name || data.Entry.Revision != "6" {
			t.Fatal("首次恢复与已提交事实不符", data, entry, err)
		}
		record, err := readRecord(key)
		const retention = 8 * 24 * time.Hour
		if err != nil || record == nil || !bytes.Equal(first, record.ResponseBody) ||
			record.ExpiresAt.Before(beforeCall.Add(retention)) || record.ExpiresAt.After(afterCall.Add(retention)) {
			t.Fatal("稳定正文或八天保留期不符", err)
		}
		for _, scenario := range []struct {
			key               string
			entryID, revision uint64
			expected          error
		}{
			{key, 15, 6, domain.ErrIdempotencyRequestConflict},
			{key, 13, 3, domain.ErrIdempotencyRequestConflict},
			{"restore-new", 15, 6, domain.ErrFileEntryNotFound},
		} {
			if body, err := restore.Restore(ctx, actor, scenario.entryID, scenario.revision, scenario.key); body != nil || !errors.Is(err, scenario.expected) {
				t.Fatal("同键异参或新键错误分类不符", err)
			}
		}
		// 布置后续改名和再次回收，核对已有结果继续使用第一次写入的数据。
		if _, err := pool.ExecContext(ctx, `UPDATE file_entries SET name='后来改名.md',name_key=CAST('后来改名.md' AS BINARY),
			status='TRASHED',deleted_at=UTC_TIMESTAMP(6),revision=7 WHERE user_id=1 AND id=15`); err != nil {
			t.Fatal(err)
		}
		current := readEntry(t, 15)
		replayed, err := restore.Restore(ctx, actor, 15, 5, key)
		if err != nil || !bytes.Equal(first, replayed) || !reflect.DeepEqual(readEntry(t, 15), current) {
			t.Fatal("重试未复用首次快照，或改变了当前记录", err)
		}
		if _, err := pool.ExecContext(ctx, `UPDATE idempotency_records
			SET created_at='2000-01-01 00:00:00', expires_at='2000-01-09 00:00:00'
			WHERE user_id=? AND operation=? AND idempotency_key=?`, actor.UserID, domain.RestoreEntryOperation, key); err != nil {
			t.Fatal(err)
		}
		if body, err := restore.Restore(ctx, actor, 15, 6, key); body != nil || !errors.Is(err, domain.ErrIdempotencyRecordExpired) {
			t.Fatal("到期记录没有优先拒绝重试", err)
		}
	})

	// 3. 在目录项和快照都已写入后制造失败，验证两条记录一起回滚。
	t.Run("snapshot save failure rolls back both writes", func(t *testing.T) {
		const key = "restore-rollback"
		before := readEntry(t, 13)
		failure := errors.New("fixture failure after saving a snapshot")
		broken := service.NewIdempotentRestoreEntryService(
			func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentRestoreEntryTransaction) error) error {
				return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
					return action(txContext, restoreSaveFailureStore{Store: txStore, failure: failure})
				})
			}, encode, validate,
		)
		body, err := broken.Restore(ctx, actor, 13, 3, key)
		record, queryError := readRecord(key)
		if body != nil || !errors.Is(err, failure) || !reflect.DeepEqual(readEntry(t, 13), before) ||
			record != nil || !errors.Is(queryError, domain.ErrIdempotencyRecordNotFound) {
			t.Fatal("快照失败没有原子回滚", err, queryError)
		}
	})

	// 4. 两个连接并发使用回滚过的键时，均得到同一份成功结果。
	t.Run("same key concurrent retry", func(t *testing.T) {
		const key = "restore-rollback"
		pool.SetMaxOpenConns(2)
		defer pool.SetMaxOpenConns(1)
		start := make(chan struct{})
		type response struct {
			body json.RawMessage
			err  error
		}
		results := make(chan response, 2)
		for range 2 {
			go func() {
				<-start
				body, err := restore.Restore(ctx, actor, 13, 3, key)
				results <- response{body: body, err: err}
			}()
		}
		close(start)
		first, second := <-results, <-results
		record, err := readRecord(key)
		entry := readEntry(t, 13)
		if first.err != nil || second.err != nil || err != nil || record == nil ||
			!bytes.Equal(first.body, second.body) || !bytes.Equal(first.body, record.ResponseBody) ||
			entry.Status != "ACTIVE" || entry.Revision != 4 {
			t.Fatal("并发重试未共享首次恢复结果", first.err, second.err, err)
		}
	})
}
