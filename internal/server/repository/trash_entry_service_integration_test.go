package repository

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// invalidTrashResultStore 模拟真实写入成功后读回的结果异常。
type invalidTrashResultStore struct {
	*Store
}

func (store invalidTrashResultStore) TrashUserFileEntry(ctx context.Context, entryID, revision uint64) (*domain.TrashedFileEntry, error) {
	result, err := store.Store.TrashUserFileEntry(ctx, entryID, revision)
	if err == nil {
		result.DeletedAt = time.Time{}
	}
	return result, err
}

func TestTrashEntryServiceIntegration(t *testing.T) {
	// 1. 复用独占数据库夹具，用单连接串起已认证用户事务和服务。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(17,1,12,'活动子项',CAST('活动子项' AS BINARY),'DIR')"); err != nil {
		t.Fatal(err)
	}
	trash := service.NewTrashEntryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.TrashEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	read := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	for _, scenario := range []struct {
		name              string
		entryID, revision uint64
		want              error
	}{
		{"foreign target", 21, 1, domain.ErrFileEntryNotFound},
		{"stale revision", 13, 2, domain.ErrEntryRevisionConflict},
		{"real root", 11, 1, domain.ErrRootEntryTrash},
		{"hidden descendant", 15, 5, domain.ErrFileEntryNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result, err := trash.Trash(ctx, actor, scenario.entryID, scenario.revision)
			if result != nil || !errors.Is(err, scenario.want) {
				t.Fatal("回收目标拒绝不符", result, err)
			}
		})
	}

	// 2. 以真实仓储更新目录项，验证服务拒绝异常结果时事务回滚。
	t.Run("invalid result rolls back real update", func(t *testing.T) {
		before := read(t, 13)
		brokenResult := service.NewTrashEntryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.TrashEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return action(txContext, invalidTrashResultStore{Store: txStore})
			})
		})
		result, err := brokenResult.Trash(ctx, actor, 13, 3)
		var deletedAt sql.NullTime
		readError := pool.QueryRowContext(ctx, "SELECT deleted_at FROM file_entries WHERE user_id=1 AND id=13").Scan(&deletedAt)
		if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || readError != nil || deletedAt.Valid ||
			!reflect.DeepEqual(read(t, 13), before) {
			t.Fatal("服务拒绝异常结果后未回滚", result, err, readError)
		}
	})

	// 3. 提交后的结果与库中事实相同，目录回收后后代通过祖先检查隐藏。
	t.Run("file and directory commit", func(t *testing.T) {
		childBefore := read(t, 17)
		for _, target := range []struct {
			entryID, revision uint64
		}{{13, 3}, {12, 1}} {
			result, err := trash.Trash(ctx, actor, target.entryID, target.revision)
			if err != nil || result == nil || result.Status != "TRASHED" || result.Revision != target.revision+1 ||
				!reflect.DeepEqual(read(t, target.entryID), &result.FileEntry) {
				t.Fatal("服务交付的回收结果与已提交数据不符", result, err)
			}
			var deletedAt time.Time
			if err := pool.QueryRowContext(ctx, "SELECT deleted_at FROM file_entries WHERE user_id=? AND id=?", actor.UserID, target.entryID).
				Scan(&deletedAt); err != nil || !deletedAt.Equal(result.DeletedAt) {
				t.Fatal("服务回收时间与数据库不符", err)
			}
		}
		result, err := trash.Trash(ctx, actor, 17, 1)
		if result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) || !reflect.DeepEqual(read(t, 17), childBefore) {
			t.Fatal("祖先回收后的子项状态或可见性错误", result, err)
		}
	})
	t.Run("revoked session stops the transaction", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE user_id=? AND id=?", actor.UserID, actor.SessionID); err != nil {
			t.Fatal(err)
		}
		result, err := trash.Trash(ctx, actor, 17, 1)
		if result != nil || !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("撤销会话后未拒绝回收事务", result, err)
		}
	})
}
