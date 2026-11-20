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

func TestTrashUserFileEntryIntegration(t *testing.T) {
	// 1. 准备待回收项、活动后代、配额和文件偏好。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE upload_tasks (")
	if start < 0 || end <= start {
		t.Fatal("目录项偏好表迁移未找到")
	}
	execute := func(t *testing.T, statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备回收写入夹具失败", err)
		}
	}
	execute(t, migrations.InitialSQL[start:end])
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision)
		VALUES(17,1,12,'活动后代',CAST('活动后代' AS BINARY),'DIR',1),
		      (18,1,11,'最后一次递增',CAST('最后一次递增' AS BINARY),'DIR',18446744073709551614)`)
	execute(t, "UPDATE users SET used_storage=2048 WHERE id=1")
	execute(t, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,13,'2026-01-01 01:02:03.123456','2026-01-01 01:02:04.123456')")
	read := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal("读取目录项失败", err)
		}
		return entry
	}
	write := func(entryID, revision uint64, checkPath bool) (*domain.TrashedFileEntry, error) {
		var saved *domain.TrashedFileEntry
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if checkPath {
				if _, err := service.ValidateTrashEntryTarget(txContext, actor, entryID, revision, txStore); err != nil {
					return err
				}
			}
			var err error
			saved, err = txStore.TrashUserFileEntry(txContext, entryID, revision)
			return err
		})
		if err != nil {
			return nil, err
		}
		return saved, nil
	}

	// 2. 直接调用仓储也须在已认证事务内，且匹配归属、状态和版本。
	for _, scenario := range []struct {
		name              string
		entryID, revision uint64
		want              error
	}{
		{"real root", 11, 2, domain.ErrRootEntryTrash},
		{"wrong revision", 13, 2, domain.ErrEntryRevisionConflict},
		{"already trashed", 14, 1, domain.ErrFileEntryNotFound},
		{"foreign entry", 21, 1, domain.ErrFileEntryNotFound},
		{"missing entry", 99, 1, domain.ErrFileEntryNotFound},
		{"exhausted revision", 16, math.MaxUint64, domain.ErrEntryRevisionExhausted},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result, err := write(scenario.entryID, scenario.revision, false)
			if result != nil || !errors.Is(err, scenario.want) {
				t.Fatal("非法回收写入结果不符", result, err)
			}
			if scenario.want == domain.ErrEntryRevisionConflict {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != 3 {
					t.Fatal("未返回数据库当前版本", err)
				}
			}
		})
	}
	t.Run("requires authenticated transaction", func(t *testing.T) {
		if entry, err := store.TrashUserFileEntry(ctx, 13, 3); entry != nil || err == nil {
			t.Fatal("事务外执行了回收", entry, err)
		}
		err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
			entry, rejected := txStore.TrashUserFileEntry(txContext, 13, 3)
			if entry != nil || rejected == nil {
				t.Fatal("无认证会话的用户事务执行了回收", entry, rejected)
			}
			return rejected
		})
		if err == nil {
			t.Fatal("预期事务拒绝缺少认证身份")
		}
	})
	t.Run("writer checks current revision after path validation", func(t *testing.T) {
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := service.ValidateTrashEntryTarget(txContext, actor, 13, 3, txStore); err != nil {
				t.Fatal("初次路径检查失败", err)
			}
			if update := txStore.database.WithContext(txContext).Exec("UPDATE file_entries SET revision=4 WHERE user_id=1 AND id=13"); update.Error != nil ||
				update.RowsAffected != 1 {
				t.Fatal("未设置写前版本变化夹具", update.Error)
			}
			entry, err := txStore.TrashUserFileEntry(txContext, 13, 3)
			if entry != nil {
				t.Fatal("写入使用了已过时的前置检查版本", entry)
			}
			return err
		})
		var conflict *domain.EntryRevisionConflictError
		if !errors.As(err, &conflict) || conflict.CurrentRevision != 4 || read(t, 13).Revision != 3 {
			t.Fatal("锁定读未返回实际版本或事务未回滚", err)
		}
	})

	// 3. 外层失败会回滚状态、版本、时间和活动名称。
	t.Run("rollback after write", func(t *testing.T) {
		before := read(t, 17)
		rollback := errors.New("fixture rolls back a successful trash write")
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			result, err := txStore.TrashUserFileEntry(txContext, 17, 1)
			if err != nil || result == nil || result.Status != "TRASHED" {
				t.Fatal("事务中未完成回收", result, err)
			}
			return rollback
		})
		var deletedAt sql.NullTime
		var activeName []byte
		if queryError := pool.QueryRowContext(ctx, "SELECT deleted_at,active_name_key FROM file_entries WHERE user_id=1 AND id=17").Scan(&deletedAt, &activeName); queryError != nil ||
			!errors.Is(err, rollback) || deletedAt.Valid || string(activeName) != before.Name || !reflect.DeepEqual(read(t, 17), before) {
			t.Fatal("回收状态、版本或时间未随事务回滚", err, queryError)
		}
	})

	// 4. 普通回收释放活动名称，保留原路径、内容引用、逻辑配额和偏好。
	t.Run("file content and metadata stay attached", func(t *testing.T) {
		before := read(t, 13)
		preferences, err := store.FindUserEntryPreference(ctx, actor.UserID, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		var dbBefore time.Time
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&dbBefore); err != nil {
			t.Fatal(err)
		}
		saved, err := write(13, 3, true)
		if err != nil || saved == nil || saved.Status != "TRASHED" || saved.Revision != 4 ||
			saved.ParentID == nil || *saved.ParentID != *before.ParentID || saved.Name != before.Name ||
			!saved.CreatedAt.Equal(before.CreatedAt) || saved.FileSize != before.FileSize || saved.EntryType != before.EntryType ||
			saved.DeletedAt.Before(dbBefore) || saved.DeletedAt.Location() != time.UTC || saved.DeletedAt.Nanosecond()%1000 != 0 ||
			!saved.DeletedAt.Equal(saved.ModifiedAt) || !saved.DeletedAt.Equal(saved.UpdatedAt) ||
			!reflect.DeepEqual(read(t, 13), &saved.FileEntry) {
			t.Fatal("提交后的回收文件结果不符", saved, err)
		}
		var originalKey, activeKey []byte
		var objectID, usedBytes uint64
		var deletedAt time.Time
		queryError := pool.QueryRowContext(ctx, `SELECT name_key,active_name_key,object_id,used_storage,deleted_at
			FROM file_entries JOIN users ON users.id=file_entries.user_id WHERE file_entries.id=13`).
			Scan(&originalKey, &activeKey, &objectID, &usedBytes, &deletedAt)
		if queryError != nil || string(originalKey) != "有效.md" || activeKey != nil || objectID != 41 ||
			usedBytes != 2048 || !deletedAt.Equal(saved.DeletedAt) {
			t.Fatal("回收后的名称占用、对象引用、配额或时间不符", queryError)
		}
		after, err := store.FindUserEntryPreference(ctx, actor.UserID, before.ID)
		if err != nil || !reflect.DeepEqual(after, preferences) {
			t.Fatal("回收改变了文件偏好", err)
		}
		execute(t, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(22,1,12,'有效.md',CAST('有效.md' AS BINARY),'DIR')")
	})
	t.Run("directory leaves descendant state intact", func(t *testing.T) {
		before := read(t, 17)
		saved, err := write(12, 1, true)
		if err != nil || saved == nil || saved.Status != "TRASHED" || saved.Revision != 2 ||
			saved.EntryType != "DIR" || !reflect.DeepEqual(read(t, 17), before) {
			t.Fatal("目录回收或后代状态不符", saved, err)
		}
		if child, err := write(17, 1, true); child != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("祖先已回收的活动后代仍通过完整检查", child, err)
		}
	})

	// 5. 相同版本的并发写入只有一次成功，最后可递增版本完整保存。
	t.Run("concurrent requests on last incrementable revision", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		defer pool.SetMaxOpenConns(1)
		results := make(chan error, 2)
		start := make(chan struct{})
		for range 2 {
			go func() {
				<-start
				_, err := write(18, math.MaxUint64-1, true)
				results <- err
			}()
		}
		close(start)
		first, second := <-results, <-results
		if !((first == nil && errors.Is(second, domain.ErrFileEntryNotFound)) ||
			(second == nil && errors.Is(first, domain.ErrFileEntryNotFound))) {
			t.Fatal("并发回收未按单次成功串行化", first, second)
		}
		entry := read(t, 18)
		if entry.Revision != math.MaxUint64 || entry.Status != "TRASHED" {
			t.Fatal("最后可递增版本未被完整保存", entry)
		}
	})
}
