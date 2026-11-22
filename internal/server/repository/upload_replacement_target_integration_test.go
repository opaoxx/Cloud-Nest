package repository

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestLockUploadReplacementTargetIntegration(t *testing.T) {
	// 1. 准备两层父目录、替换目标及另一个账号的文件。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const userID, entryID, expectedRevision = 7, 31, 6
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal("准备复核夹具失败", err)
		}
	}
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES (33,7,13,'parent',X'706172656E74','DIR'),(34,7,33,'child',X'6368696C64','DIR')`)
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES (32,8,14,'foreign.bin',CAST('foreign.bin' AS BINARY),'FILE','OTHER',24,0)`)
	execute(t, "UPDATE file_entries SET parent_id=34,revision=? WHERE id=?", expectedRevision, entryID)
	execute(t, "UPDATE users SET used_storage=16777216,reserved_storage=512 WHERE id=7")
	read := func(target, revision uint64) (*domain.FileEntry, error) {
		var entry *domain.FileEntry
		err := store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			var err error
			entry, err = txStore.LockUploadReplacementTarget(txContext, target, revision)
			return err
		})
		return entry, err
	}
	checkFailure := func(t *testing.T, target, revision uint64, want error, actualRevision uint64) {
		t.Helper()
		entry, err := read(target, revision)
		if entry != nil || !errors.Is(err, want) {
			t.Fatal("替换目标拒绝结果不符", entry, err)
		}
		var conflict *domain.EntryRevisionConflictError
		if errors.As(err, &conflict) != (actualRevision != 0) ||
			(conflict != nil && conflict.CurrentRevision != actualRevision) {
			t.Fatal("当前版本未按可见性规则返回", err)
		}
	}

	t.Run("holds target and ancestor locks", func(t *testing.T) {
		// 2. 在同一事务验证锁覆盖文件、两个祖先及根目录，提交后其他连接可取锁。
		entry, err := store.LockUploadReplacementTarget(ctx, entryID, expectedRevision)
		if entry != nil || err == nil {
			t.Fatal("事务外调用未被拒绝", entry, err)
		}
		err = store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			entry, err := txStore.LockUploadReplacementTarget(txContext, entryID, expectedRevision)
			if err != nil {
				return err
			}
			if entry == nil || entry.ID != entryID || entry.UserID != userID || entry.ParentID == nil ||
				*entry.ParentID != 34 || entry.Revision != expectedRevision {
				t.Fatal("复核没有返回锁内文件快照", entry)
			}
			for _, lockedID := range []uint64{entryID, 34, 33, 13} {
				var id uint64
				var mysqlError *mysql.MySQLError
				err := pool.QueryRowContext(ctx, "SELECT id FROM file_entries WHERE id=? FOR UPDATE NOWAIT", lockedID).Scan(&id)
				if !errors.As(err, &mysqlError) || mysqlError.Number != 3572 {
					t.Fatal("事务外连接意外取得文件或祖先锁", lockedID, id, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var released uint64
		if err := pool.QueryRowContext(ctx, "SELECT id FROM file_entries WHERE id=13 FOR UPDATE NOWAIT").Scan(&released); err != nil || released != 13 {
			t.Fatal("提交后根目录锁未释放", released, err)
		}
	})

	t.Run("rejects inaccessible paths before checking version", func(t *testing.T) {
		checkFailure(t, 999, 1, domain.ErrFileEntryNotFound, 0)
		checkFailure(t, 32, 1, domain.ErrFileEntryNotFound, 0)
		checkFailure(t, 34, 9, domain.ErrFileEntryNotFile, 0)
		execute(t, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=31")
		checkFailure(t, entryID, 1, domain.ErrFileEntryNotFound, 0)
		execute(t, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=31")
		execute(t, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=33")
		checkFailure(t, entryID, 1, domain.ErrFileEntryNotFound, 0)
		checkFailure(t, 34, 9, domain.ErrFileEntryNotFound, 0)
		execute(t, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=33")
	})

	t.Run("checks the current version after a move", func(t *testing.T) {
		checkFailure(t, entryID, 5, domain.ErrEntryRevisionConflict, expectedRevision)
		execute(t, "UPDATE file_entries SET parent_id=13,revision=7 WHERE id=31")
		checkFailure(t, entryID, expectedRevision, domain.ErrEntryRevisionConflict, 7)
		entry, err := read(entryID, 7)
		if err != nil || entry == nil || entry.ParentID == nil || *entry.ParentID != 13 {
			t.Fatal("复核仍使用移动前的父目录", entry, err)
		}
		execute(t, "UPDATE file_entries SET revision=? WHERE id=31", uint64(math.MaxUint64))
		checkFailure(t, entryID, 7, domain.ErrEntryRevisionConflict, math.MaxUint64)
		checkFailure(t, entryID, math.MaxUint64, domain.ErrEntryRevisionExhausted, 0)
	})

	t.Run("cancelled read", func(t *testing.T) {
		err := store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			cancelled, stop := context.WithCancel(txContext)
			stop()
			entry, err := txStore.LockUploadReplacementTarget(cancelled, entryID, expectedRevision)
			if entry != nil {
				t.Fatal("取消后仍返回文件", entry)
			}
			return err
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal("未保留取消错误", err)
		}
	})

	// 3. 复核没有生成历史或变更事件，也没有写入任务、当前对象引用或配额。
	var state string
	if err := pool.QueryRowContext(ctx, `SELECT CONCAT_WS('|',e.object_id,e.file_size,u.used_storage,u.reserved_storage,u.change_seq,t.status,
		(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events))
		FROM file_entries e JOIN users u ON u.id=e.user_id JOIN upload_tasks t ON t.user_id=u.id WHERE e.id=31 AND t.id=19`).
		Scan(&state); err != nil || state != "29|16777216|16777216|512|0|COMPLETED|0|0" {
		t.Fatal("只读复核改变了业务记录", state, err)
	}
}
