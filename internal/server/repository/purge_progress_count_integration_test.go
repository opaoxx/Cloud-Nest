package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestCountPurgeRemainingEntriesIntegration(t *testing.T) {
	// 1. 准备清理目标、各状态后代、墓碑和原有旁支。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	for _, query := range []string{
		"UPDATE file_entries SET status='PURGING',purge_started_at='2026-01-02 03:04:05.123456' WHERE user_id=1 AND id=14",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at)
		VALUES (17,1,14,'待清理回收后代',CAST('待清理回收后代' AS BINARY),'DIR','TRASHED','2026-01-01',NULL,NULL),
		       (18,1,14,'已清理后代',CAST('已清理后代' AS BINARY),'DIR','PURGED',NULL,NULL,'2026-01-02 03:04:06.123456'),
		       (19,1,17,'正在清理的后代',CAST('正在清理的后代' AS BINARY),'DIR','PURGING',NULL,'2026-01-02 03:04:05.123456',NULL)`,
	} {
		if _, err := pool.ExecContext(ctx, query); err != nil {
			t.Fatal("准备计数夹具失败", err)
		}
	}
	checkCount := func(snapshotContext context.Context, snapshotStore *Store, expected int) error {
		target, err := service.ReadPurgeProgressTarget(snapshotContext, actor, 14, snapshotStore)
		if err != nil {
			return err
		}
		entries, err := snapshotStore.ListUserPurgePreviewSubtree(snapshotContext, target.UserID, target.ID)
		if err != nil {
			return err
		}
		count, err := service.CountPurgeRemainingEntries(target.ID, entries)
		if err != nil {
			return err
		}
		if count != expected {
			return fmt.Errorf("子树还剩 %d 项，预期 %d 项", count, expected)
		}
		return nil
	}

	// 2. 在同一快照核对计数，用另一连接提交一个文件的完成状态。
	if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
		if err := checkCount(snapshotContext, snapshotStore, 4); err != nil {
			return err
		}
		_, err := pool.ExecContext(snapshotContext, `UPDATE file_entries
			SET status='PURGED',object_id=NULL,purged_at='2026-01-02 03:04:06.123456',revision=revision+1
			WHERE user_id=1 AND id=15`)
		if err != nil {
			return err
		}
		return checkCount(snapshotContext, snapshotStore, 4)
	}); err != nil {
		t.Fatal("原快照中的计数应保持一致", err)
	}

	// 3. 在新快照看到已完成项减少，再验证根目标自身仍占一项。
	if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
		return checkCount(snapshotContext, snapshotStore, 3)
	}); err != nil {
		t.Fatal("新快照未反映已完成后代", err)
	}
	for _, entryID := range []uint64{19, 17} {
		if _, err := pool.ExecContext(ctx, `UPDATE file_entries
			SET status='PURGED',purged_at='2026-01-02 03:04:07.123456',revision=revision+1
			WHERE user_id=1 AND id=?`, entryID); err != nil {
			t.Fatal("推进目录夹具清理完成失败", err)
		}
	}
	if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
		return checkCount(snapshotContext, snapshotStore, 1)
	}); err != nil {
		t.Fatal("未完成的根目标应计为一项", err)
	}
}
