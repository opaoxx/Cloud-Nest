package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMarkPurgeLeafCompletedIntegration(t *testing.T) {
	// 1. 准备已受理根项、文件和空目录，从真实候选启动用户事务。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	deletedAt := startedAt.Add(-time.Minute)
	if _, err := pool.ExecContext(ctx, `UPDATE file_entries SET status='PURGING',purge_started_at=?,deleted_at=?,revision=?
		WHERE user_id=1 AND id=14`, startedAt, deletedAt, uint64(math.MaxUint64)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `UPDATE file_entries SET status='TRASHED',deleted_at=?
		WHERE user_id=1 AND id=15`, deletedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES (17,1,14,'空目录',CAST('空目录' AS BINARY),'DIR')`); err != nil {
		t.Fatal(err)
	}
	roots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(roots) != 1 || roots[0].EntryID != 14 || roots[0].Revision != math.MaxUint64 {
		t.Fatal("未读取到已受理的最高版本根项", roots, err)
	}
	root := roots[0]
	rollbackFixture := errors.New("仅验证内部写入，回滚隔离库夹具")

	err = store.WithPurgeRootTransaction(ctx, root, func(txContext context.Context, txStore *Store) error {
		// 2. 锁定叶子，确认换过版本或对象引用的写入不会成功。
		candidates, err := txStore.ListPurgeLeafCandidates(txContext, 10)
		if err != nil {
			return err
		}
		if len(candidates) != 2 || candidates[0].EntryID != 15 || candidates[1].EntryID != 17 {
			return fmt.Errorf("叶子候选不符：%+v", candidates)
		}
		file, err := txStore.LockPurgeLeaf(txContext, candidates[0])
		if err != nil {
			return err
		}
		wrongObjectID := uint64(42)
		wrongObject, wrongRevision := *file, *file
		wrongObject.ObjectID = &wrongObjectID
		wrongRevision.Revision++
		for _, stale := range []*domain.LockedPurgeLeaf{&wrongObject, &wrongRevision} {
			if err := txStore.markPurgeLeafCompleted(txContext, stale); !errors.Is(err, domain.ErrPurgeLeafCandidateStale) {
				return fmt.Errorf("条件写入接受了失效候选：%v", err)
			}
		}

		// 3. 逐项写入完成状态，读回时间、版本和已清空的对象引用。
		verifyWrite := func(leaf *domain.LockedPurgeLeaf, nextRevision uint64, expectedStarted, expectedDeleted *time.Time) error {
			if err := txStore.markPurgeLeafCompleted(txContext, leaf); err != nil {
				return err
			}
			if err := txStore.markPurgeLeafCompleted(txContext, leaf); !errors.Is(err, domain.ErrPurgeLeafCandidateStale) {
				return fmt.Errorf("相同候选重复完成未被拒绝：%v", err)
			}
			var status string
			var revision, fileSize, parentID uint64
			var objectID sql.Null[uint64]
			var started, deleted, completed sql.NullTime
			var modified, updated time.Time
			err := txStore.database.WithContext(txContext).Raw(`
				SELECT status,revision,file_size,parent_id,object_id,purge_started_at,deleted_at,purged_at,modified_at,updated_at
				FROM file_entries WHERE user_id=? AND id=?`, leaf.UserID, leaf.EntryID).Row().
				Scan(&status, &revision, &fileSize, &parentID, &objectID, &started, &deleted, &completed, &modified, &updated)
			if err != nil {
				return err
			}
			if status != "PURGED" || revision != nextRevision || fileSize != leaf.FileSize || parentID != leaf.ParentID ||
				objectID.Valid || !completed.Valid || completed.Time.Before(startedAt) || completed.Time.Nanosecond()%1000 != 0 ||
				!completed.Time.Equal(modified) || !completed.Time.Equal(updated) {
				return fmt.Errorf("目录项 %d 的完成状态或时间不符", leaf.EntryID)
			}
			if started.Valid != (expectedStarted != nil) || (expectedStarted != nil && !started.Time.Equal(*expectedStarted)) ||
				deleted.Valid != (expectedDeleted != nil) || (expectedDeleted != nil && !deleted.Time.Equal(*expectedDeleted)) {
				return fmt.Errorf("目录项 %d 的原清理开始或回收时间变化", leaf.EntryID)
			}
			return nil
		}
		if err := verifyWrite(file, file.Revision+1, nil, &deletedAt); err != nil {
			return err
		}
		directory, err := txStore.LockPurgeLeaf(txContext, candidates[1])
		if err != nil {
			return err
		}
		if err := verifyWrite(directory, directory.Revision+1, nil, nil); err != nil {
			return err
		}
		remaining, err := txStore.ListPurgeLeafCandidates(txContext, 1)
		if err != nil {
			return err
		}
		if len(remaining) != 1 || remaining[0].EntryID != root.EntryID {
			return fmt.Errorf("子项完成后未选中根项：%+v", remaining)
		}
		lockedRoot, err := txStore.LockPurgeLeaf(txContext, remaining[0])
		if err != nil {
			return err
		}
		if err := verifyWrite(lockedRoot, math.MaxUint64, &startedAt, &deletedAt); err != nil {
			return err
		}
		return rollbackFixture
	})
	if !errors.Is(err, rollbackFixture) {
		t.Fatal("写入验证未完整执行或未触发回滚", err)
	}

	// 4. 回查事务外状态，确认所有中间写入一起回滚。
	after, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(after) != 1 || after[0] != root {
		t.Fatal("根项完成结果未回滚", after, err)
	}
	content, err := store.FindUserFileEntryContent(ctx, 1, 15)
	if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != 41 {
		t.Fatal("文件引用未回滚", content, err)
	}
}
