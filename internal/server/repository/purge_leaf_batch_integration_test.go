package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCompletePurgeLeafBatchIntegration(t *testing.T) {
	// 1. 在同一个清理根目录下准备两个文件和一个空目录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store := purgeLeafSettlementTestDatabase(t, ctx)
	execute := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备批次结算夹具失败", err)
		}
	}
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=UTC_TIMESTAMP(6),revision=revision+1 WHERE id=14")
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES (17,1,14,'待清理.md',CAST('待清理.md' AS BINARY),'FILE','DOCUMENT',41,2048)`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES (18,1,14,'空目录',CAST('空目录' AS BINARY),'DIR')`)
	execute(`INSERT INTO user_entry_preferences(user_id,entry_id,starred_at)
		SELECT user_id,id,UTC_TIMESTAMP(6) FROM file_entries WHERE user_id=1 AND id IN (13,14,15,17,18)`)
	execute("UPDATE users SET storage_quota=10000,used_storage=2048,reserved_storage=512 WHERE id=1")

	roots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(roots) != 1 || roots[0].EntryID != 14 {
		t.Fatal("未取得批次夹具的清理根项", roots, err)
	}
	root := roots[0]
	assertEntry := func(entryID uint64, wantStatus string, wantRevision, wantObjectID uint64) {
		t.Helper()
		var status string
		var revision, objectID uint64
		var hasCompletionTime, hasPreference bool
		err := pool.QueryRowContext(ctx, `
			SELECT entry.status,entry.revision,COALESCE(entry.object_id,0),entry.purged_at IS NOT NULL,
			       EXISTS(SELECT 1 FROM user_entry_preferences WHERE user_id=entry.user_id AND entry_id=entry.id)
			FROM file_entries AS entry WHERE entry.user_id=1 AND entry.id=?`, entryID).
			Scan(&status, &revision, &objectID, &hasCompletionTime, &hasPreference)
		wantCompleted := wantStatus == "PURGED"
		if err != nil || status != wantStatus || revision != wantRevision || objectID != wantObjectID ||
			hasCompletionTime != wantCompleted || hasPreference != !wantCompleted {
			t.Fatalf("目录项 %d 实际状态 %s，版本 %d，引用 %d，完成时间存在 %t，偏好存在 %t，错误 %v",
				entryID, status, revision, objectID, hasCompletionTime, hasPreference, err)
		}
	}
	assertQuota := func(wantUsed uint64) {
		t.Helper()
		var used, reserved uint64
		err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=1").Scan(&used, &reserved)
		if err != nil || used != wantUsed || reserved != 512 {
			t.Fatalf("已用空间为 %d，期望 %d，预占空间为 %d，错误 %v", used, wantUsed, reserved, err)
		}
	}

	// 2. 拒绝越界的批量大小，模拟第二个文件配额不足，回查整批均未提交。
	for _, limit := range []int{0, domain.MaxPurgeLeafCandidates + 1} {
		if count, err := store.CompletePurgeLeafBatch(ctx, root, limit); err == nil || count != 0 {
			t.Fatal("接受了越界的批量大小", limit, count, err)
		}
	}
	count, err := store.CompletePurgeLeafBatch(ctx, root, 2)
	if count != 0 || !errors.Is(err, domain.ErrInvalidAccountData) || !strings.Contains(err.Error(), "叶子 17 失败") {
		t.Fatal("第二个文件配额不足时应返回零和原始错误", count, err)
	}
	assertQuota(2048)
	assertEntry(14, "PURGING", root.Revision, 0)
	assertEntry(15, "ACTIVE", 5, 41)
	assertEntry(17, "ACTIVE", 1, 41)
	assertEntry(18, "ACTIVE", 1, 0)

	// 3. 恢复账目，先按上限结算一个文件，再在同一批结算另外两个叶子。
	execute("UPDATE users SET used_storage=6144 WHERE id=1")
	if count, err := store.CompletePurgeLeafBatch(ctx, root, 1); err != nil || count != 1 {
		t.Fatal("限制为一项时应只提交一个叶子", count, err)
	}
	assertQuota(4096)
	assertEntry(15, "PURGED", 6, 0)
	assertEntry(17, "ACTIVE", 1, 41)
	assertEntry(18, "ACTIVE", 1, 0)

	if count, err := store.CompletePurgeLeafBatch(ctx, root, domain.MaxPurgeLeafCandidates); err != nil || count != 2 {
		t.Fatal("第二批应提交剩余的两个叶子", count, err)
	}
	assertQuota(2048)
	assertEntry(17, "PURGED", 2, 0)
	assertEntry(18, "PURGED", 2, 0)
	assertEntry(14, "PURGING", root.Revision, 0)

	// 4. 后代完成后的下一批才结算根项，原根项候选随之失效。
	if count, err := store.CompletePurgeLeafBatch(ctx, root, domain.MaxPurgeLeafCandidates); err != nil || count != 1 {
		t.Fatal("最后一批应仅完成根项", count, err)
	}
	assertEntry(14, "PURGED", root.Revision+1, 0)
	var startedAt time.Time
	err = pool.QueryRowContext(ctx, "SELECT purge_started_at FROM file_entries WHERE id=14").Scan(&startedAt)
	if err != nil || !startedAt.Equal(root.PurgeStartedAt) {
		t.Fatal("根项原清理开始时间应保留", startedAt, err)
	}
	if count, err := store.CompletePurgeLeafBatch(ctx, root, 1); count != 0 || !errors.Is(err, domain.ErrPurgeRootCandidateStale) {
		t.Fatal("已完成的根项应拒绝重复结算", count, err)
	}
	assertQuota(2048)
	assertEntry(13, "ACTIVE", 3, 41)
}
