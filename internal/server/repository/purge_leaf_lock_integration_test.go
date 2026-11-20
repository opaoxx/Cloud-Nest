package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestLockPurgeLeafIntegration(t *testing.T) {
	// 1. 建立清理根项、可处理文件、嵌套目录和跨账号干扰项。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备锁定叶子的夹具失败", err)
		}
	}
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	finishedAt := startedAt.Add(time.Second)
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=?,revision=revision+1 WHERE id=14", startedAt)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES (17,1,14,'普通父目录',CAST('普通父目录' AS BINARY),'DIR'),
		       (18,1,17,'嵌套空目录',CAST('嵌套空目录' AS BINARY),'DIR'),
		       (22,2,21,'另一账号目录',CAST('另一账号目录' AS BINARY),'DIR')`)
	roots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(roots) != 1 || roots[0].EntryID != 14 {
		t.Fatal("未取得预期清理根项", roots, err)
	}
	root := roots[0]
	file := domain.PurgeLeafCandidate{EntryID: 15, ParentID: 14, EntryType: "FILE", Status: "ACTIVE", Revision: 5, FileSize: 2048}
	directory := domain.PurgeLeafCandidate{EntryID: 18, ParentID: 17, EntryType: "DIR", Status: "ACTIVE", Revision: 1}

	// 2. 用真实候选锁定当前记录，核对对象引用和行锁。
	if err := store.WithPurgeRootTransaction(ctx, root, func(txContext context.Context, txStore *Store) error {
		candidates, err := txStore.ListPurgeLeafCandidates(txContext, 2)
		if err != nil {
			return err
		}
		if len(candidates) != 2 || candidates[0] != file || candidates[1] != directory {
			return fmt.Errorf("叶子候选不符：%+v", candidates)
		}
		lockedFile, err := txStore.LockPurgeLeaf(txContext, candidates[0])
		if err != nil {
			return err
		}
		if lockedFile.PurgeLeafCandidate != file || lockedFile.UserID != actor.UserID ||
			lockedFile.ObjectID == nil || *lockedFile.ObjectID != 41 {
			return fmt.Errorf("文件内容引用或锁定记录错误：%+v", lockedFile)
		}
		lockedDirectory, err := txStore.LockPurgeLeaf(txContext, candidates[1])
		if err != nil {
			return err
		}
		if lockedDirectory.PurgeLeafCandidate != directory || lockedDirectory.ObjectID != nil {
			return fmt.Errorf("目录锁定记录错误：%+v", lockedDirectory)
		}
		contender, err := pool.BeginTx(txContext, nil)
		if err != nil {
			return err
		}
		defer contender.Rollback()
		var id uint64
		err = contender.QueryRowContext(txContext, "SELECT id FROM file_entries WHERE id=15 FOR UPDATE NOWAIT").Scan(&id)
		var lockError *mysql.MySQLError
		if !errors.As(err, &lockError) || lockError.Number != 3572 {
			return fmt.Errorf("另一事务未被叶子行锁拒绝：%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if locked, err := store.LockPurgeLeaf(ctx, file); locked != nil || err == nil {
		t.Fatal("普通 Store 可以锁定叶子", locked, err)
	}

	// 3. 变更候选、跨范围、跨账号和还有子项的记录都应失败。
	staleVersion := file
	staleVersion.Revision++
	for _, scenario := range []struct {
		name      string
		candidate domain.PurgeLeafCandidate
	}{
		{"changed revision", staleVersion},
		{"waiting parent", domain.PurgeLeafCandidate{EntryID: 17, ParentID: 14, EntryType: "DIR", Status: "ACTIVE", Revision: 1}},
		{"outside subtree", domain.PurgeLeafCandidate{EntryID: 13, ParentID: 12, EntryType: "FILE", Status: "ACTIVE", Revision: 3, FileSize: 2048}},
		{"foreign entry", domain.PurgeLeafCandidate{EntryID: 22, ParentID: 21, EntryType: "DIR", Status: "ACTIVE", Revision: 1}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			err := store.WithPurgeRootTransaction(ctx, root, func(txContext context.Context, txStore *Store) error {
				locked, err := txStore.LockPurgeLeaf(txContext, scenario.candidate)
				if locked != nil {
					return fmt.Errorf("失效候选返回了锁定记录")
				}
				return err
			})
			if !errors.Is(err, domain.ErrPurgeLeafCandidateStale) {
				t.Fatal("未识别失效候选", err)
			}
		})
	}

	// 4. 完成后代后可以锁定根项目录，已完成的文件候选失效。
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=? WHERE id IN (15,18,17)", finishedAt)
	err = store.WithPurgeRootTransaction(ctx, root, func(txContext context.Context, txStore *Store) error {
		if locked, err := txStore.LockPurgeLeaf(txContext, file); locked != nil || !errors.Is(err, domain.ErrPurgeLeafCandidateStale) {
			return fmt.Errorf("已完成的文件仍可锁定：%v", err)
		}
		candidate := domain.PurgeLeafCandidate{EntryID: 14, ParentID: 11, EntryType: "DIR", Status: "PURGING", Revision: 2}
		locked, err := txStore.LockPurgeLeaf(txContext, candidate)
		if err != nil {
			return err
		}
		if locked.PurgeLeafCandidate != candidate || locked.ObjectID != nil {
			return fmt.Errorf("根项已成为叶子时锁定记录不符")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
