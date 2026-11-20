package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestValidatePurgePreviewTargetIntegration(t *testing.T) {
	// 1. 准备顶层回收项、嵌套回收项及清理中的目录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	for _, statement := range []string{
		"UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000' WHERE user_id=1 AND id IN (13,15)",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at)
		  VALUES(17,1,11,'清理中目录',CAST('清理中目录' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),NULL),
		        (18,1,11,'已清理目录',CAST('已清理目录' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		  VALUES(22,1,17,'嵌套回收目录',CAST('嵌套回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备预检目标夹具失败", err)
		}
	}
	check := func(entryID uint64) (*domain.FileEntry, error) {
		var target *domain.FileEntry
		err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			var readError error
			target, readError = service.ValidatePurgePreviewTarget(snapshotContext, actor, entryID, snapshotStore)
			return readError
		})
		if err != nil {
			return nil, err
		}
		return target, nil
	}

	// 2. 通过单连接只读快照验证归属、状态、父链及实际记录。
	for _, scenario := range []struct {
		name    string
		entryID uint64
		want    error
	}{
		{"expired but still trashed file", 13, nil},
		{"top-level directory", 14, nil},
		{"own root", 11, domain.ErrRootEntryPurge},
		{"active item", 12, domain.ErrFileEntryNotFound},
		{"nested trashed item", 15, domain.ErrFileEntryNotFound},
		{"purging target", 17, domain.ErrFileEntryNotFound},
		{"purged target", 18, domain.ErrFileEntryNotFound},
		{"purging ancestor", 22, domain.ErrFileEntryNotFound},
		{"foreign root", 21, domain.ErrFileEntryNotFound},
		{"missing item", 99, domain.ErrFileEntryNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			target, err := check(scenario.entryID)
			if !errors.Is(err, scenario.want) || (target == nil) != (scenario.want != nil) {
				t.Fatalf("预检目标 %+v，错误 %v，期望 %v", target, err, scenario.want)
			}
			if target != nil {
				stored, err := store.FindUserFileEntry(ctx, actor.UserID, target.ID)
				if err != nil || !reflect.DeepEqual(target, stored) {
					t.Fatal("预检结果与数据库记录不符", target, stored, err)
				}
			}
		})
	}

	// 3. 真正提交一次并发父目录状态变更，验证该变化在下一份快照才可见。
	t.Run("parent change appears only in new snapshot", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		defer pool.SetMaxOpenConns(1)
		changed := false
		var target *domain.FileEntry
		err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			reader := &fileEntryPathChangingReader{Store: snapshotStore, afterRead: func(entryID uint64) error {
				if entryID != 13 || changed {
					return nil
				}
				result, err := pool.ExecContext(snapshotContext, `UPDATE file_entries
					SET status='TRASHED', deleted_at=UTC_TIMESTAMP(6), revision=revision+1
					WHERE user_id=1 AND id=12 AND status='ACTIVE'`)
				if err != nil {
					return err
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					return fmt.Errorf("父目录夹具更新影响 %d 条记录: %v", count, err)
				}
				changed = true
				return nil
			}}
			var readError error
			target, readError = service.ValidatePurgePreviewTarget(snapshotContext, actor, 13, reader)
			return readError
		})
		if err != nil || !changed || target == nil || target.ID != 13 {
			t.Fatal("当前快照未保留父目录的原活动状态", target, err)
		}
		if target, err := check(13); target != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("新快照未按已回收祖先隐藏目标", target, err)
		}
	})
}
