package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestRestoreUserFileEntryIntegration(t *testing.T) {
	// 1. 准备可恢复文件、目录与不同清理状态，以及内容和偏好事实。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	preferencesStart := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	preferencesEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE upload_tasks (")
	if preferencesStart < 0 || preferencesEnd <= preferencesStart {
		t.Fatal("目录项偏好表迁移未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[preferencesStart:preferencesEnd],
		"UPDATE users SET used_storage=4096 WHERE id=1",
		`UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000',
		    modified_at='2000-01-01 00:00:00.000000', updated_at='2000-01-01 00:00:00.000000'
		  WHERE user_id=1 AND id IN (13,15,16)`,
		"UPDATE file_entries SET extension='md', mime_type='text/markdown', content_modified_at='2000-01-01 00:00:00.000000' WHERE id=15",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,15,'2000-01-01 01:02:03.123456','2000-01-01 01:02:04.123456')",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision)
		  VALUES(17,1,11,'回收文件夹',CAST('回收文件夹' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),1),
		        (18,1,11,'最后一次递增',CAST('最后一次递增' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),18446744073709551614),
		        (22,1,17,'活动后代',CAST('活动后代' AS BINARY),'DIR','ACTIVE',NULL,1),
		        (23,1,17,'回收后代',CAST('回收后代' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),1),
		        (27,1,11,'隐藏.md',CAST('隐藏.md' AS BINARY),'DIR','ACTIVE',NULL,1)`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at)
		  VALUES(24,1,11,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),NULL),
		        (25,1,11,'已清理',CAST('已清理' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备恢复写入夹具失败", err)
		}
	}
	read := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		record, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal("读取目录项失败", err)
		}
		return record
	}
	write := func(entryID, revision, parentID uint64, name string) (*domain.FileEntry, error) {
		key, err := service.BuildEntryNameKey(name)
		if err != nil {
			return nil, err
		}
		var restored *domain.FileEntry
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var writeError error
			restored, writeError = txStore.RestoreUserFileEntry(txContext, entryID, revision, parentID, name, key)
			return writeError
		})
		if err != nil {
			return nil, err
		}
		return restored, nil
	}

	// 2. 在仓储边界复查事务、归属、状态和版本，拒绝不合条件的写入。
	for _, scenario := range []struct {
		name                     string
		entryID, version, parent uint64
		want                     error
	}{
		{"root", 11, 2, 11, domain.ErrRootEntryRestore},
		{"stale version", 13, 2, 12, domain.ErrEntryRevisionConflict},
		{"foreign entry", 21, 1, 11, domain.ErrFileEntryNotFound},
		{"foreign parent", 13, 3, 21, domain.ErrFileEntryNotFound},
		{"missing entry", 99, 1, 11, domain.ErrFileEntryNotFound},
		{"active source", 12, 1, 11, domain.ErrFileEntryNotFound},
		{"purging source", 24, 1, 11, domain.ErrFileEntryNotFound},
		{"purged source", 25, 1, 11, domain.ErrFileEntryNotFound},
		{"exhausted version", 16, math.MaxUint64, 11, domain.ErrEntryRevisionExhausted},
		{"inactive original parent", 15, 5, 14, domain.ErrRestoreTargetUnavailable},
		{"unrelated active directory", 13, 3, 27, domain.ErrInvalidFileEntryData},
		{"self parent", 17, 1, 17, domain.ErrInvalidFileEntryData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if result, err := write(scenario.entryID, scenario.version, scenario.parent, "恢复"); result != nil || !errors.Is(err, scenario.want) {
				t.Fatalf("拒绝结果为 %+v，错误为 %v，期望 %v", result, err, scenario.want)
			} else if errors.Is(err, domain.ErrEntryRevisionConflict) {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != 3 {
					t.Fatal("未返回锁定读取得的当前版本", err)
				}
			}
		})
	}
	t.Run("requires authenticated transaction", func(t *testing.T) {
		if record, err := store.RestoreUserFileEntry(ctx, 13, 3, 12, "笔记", []byte("笔记")); record != nil || err == nil {
			t.Fatal("事务外调用没有被拒绝", record, err)
		}
		err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
			record, err := txStore.RestoreUserFileEntry(txContext, 13, 3, 12, "笔记", []byte("笔记"))
			if record != nil || err == nil {
				t.Fatal("仅锁定用户的事务执行了恢复", record, err)
			}
			return err
		})
		if err == nil {
			t.Fatal("预期恢复因缺少认证身份被拒绝")
		}
	})
	t.Run("active name constraint and rollback", func(t *testing.T) {
		before := read(t, 15)
		if record, err := write(15, 5, 11, "隐藏.md"); record != nil || !errors.Is(err, domain.ErrEntryNameConflict) {
			t.Fatal("活动名称唯一键未产生名称冲突", record, err)
		}
		rollback := errors.New("fixture rolls back restore")
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			record, err := txStore.RestoreUserFileEntry(txContext, 15, 5, 11, "重试.md", []byte("重试.md"))
			if err != nil || record == nil || record.Status != "ACTIVE" {
				t.Fatal("回滚前没有写入恢复状态", record, err)
			}
			return rollback
		})
		var deletedAt sql.NullTime
		var activeKey []byte
		queryError := pool.QueryRowContext(ctx, "SELECT deleted_at,active_name_key FROM file_entries WHERE id=15").Scan(&deletedAt, &activeKey)
		if !errors.Is(err, rollback) || queryError != nil || !deletedAt.Valid || activeKey != nil || !reflect.DeepEqual(read(t, 15), before) {
			t.Fatal("外层回滚后回收时间、活动键或目录事实错误", err, queryError)
		}
	})

	// 3. 串联恢复前置检查及写入，核对提交后的内容、偏好与后代。
	t.Run("file uses root and numbered name", func(t *testing.T) {
		before := read(t, 15)
		preference, err := store.FindUserEntryPreference(ctx, actor.UserID, 15)
		if err != nil {
			t.Fatal(err)
		}
		var restored *domain.FileEntry
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			target, err := service.ReadRestoreEntryTarget(txContext, actor, 15, txStore)
			if err != nil {
				return err
			}
			ancestors, err := service.ReadRestoreEntryAncestors(txContext, actor, target, txStore)
			if err != nil {
				return err
			}
			if err := service.ValidateRestoreEntryRevision(target.Revision, before.Revision); err != nil {
				return err
			}
			stats, err := txStore.FindUserEntrySubtreeStats(txContext, actor.UserID, target.ID)
			if err != nil {
				return err
			}
			parentID, err := service.SelectRestoreEntryParent(target, ancestors, stats)
			if err != nil {
				return err
			}
			name, err := service.ResolveRestoreEntryName(txContext, target, parentID, txStore.FindUserActiveFileEntryByName)
			if err != nil {
				return err
			}
			key, err := service.BuildEntryNameKey(name)
			if err != nil {
				return err
			}
			restored, err = txStore.RestoreUserFileEntry(txContext, target.ID, before.Revision, parentID, name, key)
			return err
		})
		if err != nil || restored == nil || restored.Name != "隐藏 (1).md" || restored.Status != "ACTIVE" || restored.Revision != 6 ||
			restored.ParentID == nil || *restored.ParentID != 11 || !restored.ModifiedAt.After(before.ModifiedAt) ||
			!restored.UpdatedAt.Equal(restored.ModifiedAt) || !restored.CreatedAt.Equal(before.CreatedAt) || !reflect.DeepEqual(read(t, 15), restored) {
			t.Fatal("提交后的文件恢复结果错误", restored, err)
		}
		var activeKey []byte
		var objectID, usedBytes uint64
		var extension, category, mimeType string
		var deletedAt sql.NullTime
		var contentTime time.Time
		queryError := pool.QueryRowContext(ctx, `SELECT active_name_key,object_id,used_storage,extension,category,mime_type,deleted_at,content_modified_at
			FROM file_entries JOIN users ON users.id=file_entries.user_id WHERE file_entries.id=15`).
			Scan(&activeKey, &objectID, &usedBytes, &extension, &category, &mimeType, &deletedAt, &contentTime)
		if queryError != nil || string(activeKey) != "隐藏 (1).md" || objectID != 41 || usedBytes != 4096 || extension != "md" ||
			category != "DOCUMENT" || mimeType != "text/markdown" || deletedAt.Valid || !contentTime.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatal("活动键、内容、配额或回收时间与契约不符", queryError)
		}
		after, err := store.FindUserEntryPreference(ctx, actor.UserID, 15)
		if err != nil || !reflect.DeepEqual(preference, after) {
			t.Fatal("恢复改变了文件偏好", err)
		}
	})
	t.Run("directory keeps descendants", func(t *testing.T) {
		activeChild, trashedChild := read(t, 22), read(t, 23)
		restored, err := write(17, 1, 11, "回收文件夹")
		if err != nil || restored == nil || restored.Status != "ACTIVE" || restored.Revision != 2 ||
			!reflect.DeepEqual(read(t, 22), activeChild) || !reflect.DeepEqual(read(t, 23), trashedChild) {
			t.Fatal("目录恢复结果或后代状态错误", restored, err)
		}
	})
	t.Run("original non-root parent", func(t *testing.T) {
		restored, err := write(13, 3, 12, "有效.md")
		if err != nil || restored == nil || restored.ParentID == nil || *restored.ParentID != 12 ||
			restored.Status != "ACTIVE" || restored.Revision != 4 {
			t.Fatal("恢复到原父目录失败", restored, err)
		}
	})

	// 4. 同版本并发恢复只提交一次，验证最后一个可递增版本。
	t.Run("concurrent requests on last incrementable version", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		defer pool.SetMaxOpenConns(1)
		start := make(chan struct{})
		results := make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				_, err := write(18, math.MaxUint64-1, 11, "最后一次递增")
				results <- err
			}()
		}
		close(start)
		successful, unavailable := 0, 0
		for range 2 {
			switch err := <-results; {
			case err == nil:
				successful++
			case errors.Is(err, domain.ErrFileEntryNotFound):
				unavailable++
			default:
				t.Fatal(err)
			}
		}
		saved := read(t, 18)
		if successful != 1 || unavailable != 1 || saved.Revision != math.MaxUint64 || saved.Status != "ACTIVE" {
			t.Fatal("并发恢复未只提交一次，或最大版本保存错误", successful, unavailable, saved)
		}
	})
}
