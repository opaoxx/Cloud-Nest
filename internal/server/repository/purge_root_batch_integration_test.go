package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestPurgeRootBatchIntegration(t *testing.T) {
	// 1. 准备两个用户的根项、被上层覆盖的根项及无法扣减配额的文件根项。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store := purgeLeafSettlementTestDatabase(t, ctx)
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备永久清理单轮夹具失败", err)
		}
	}
	execute("UPDATE users SET storage_quota=10000,used_storage=2048 WHERE id=1")
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=?,revision=revision+1 WHERE id=14", startedAt)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at)
		VALUES (17,1,14,'独立受理子目录',CAST('独立受理子目录' AS BINARY),'DIR','PURGING',?),
		       (22,2,21,'另一用户待清理',CAST('另一用户待清理' AS BINARY),'DIR','PURGING',?)`,
		startedAt.Add(time.Second), startedAt.Add(3*time.Second))
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=?,revision=revision+1 WHERE id=13",
		startedAt.Add(2*time.Second))

	// 2. 用真实 Store 执行一轮，核对过时的候选和单根失败不会阻塞后面的用户。
	batch := service.NewPurgeRootBatch(store.ListPurgeRootCandidates, store.CompletePurgeLeafBatch)
	summary, err := batch.RunOnce(ctx, 4, 2)
	if err != nil || summary.Scanned != 4 || summary.CompletedLeaves != 3 || summary.StaleRoots != 1 ||
		len(summary.CandidateErrors) != 1 {
		t.Fatal("清理单轮的汇总不符", summary, err)
	}
	failure := summary.CandidateErrors[0]
	if failure.UserID != 1 || failure.RootID != 13 || !errors.Is(failure.Err, domain.ErrInvalidAccountData) {
		t.Fatal("失败根项没有保留原始配额错误和归属", failure)
	}
	assertStatus := func(userID, entryID uint64, want string) {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, userID, entryID)
		if err != nil || entry == nil || entry.Status != want {
			t.Fatal("目录项状态不符", userID, entryID, entry, err)
		}
	}
	assertStatus(1, 14, "PURGING")
	assertStatus(1, 15, "PURGED")
	assertStatus(1, 17, "PURGED")
	assertStatus(1, 13, "PURGING")
	assertStatus(2, 22, "PURGED")

	// 3. 下一轮重新扫描数据库，优先完成最早的根目录。
	next, err := batch.RunOnce(ctx, 1, 2)
	if err != nil || next.Scanned != 1 || next.CompletedLeaves != 1 || next.StaleRoots != 0 || len(next.CandidateErrors) != 0 {
		t.Fatal("重新扫描没有从持久进度继续", next, err)
	}
	assertStatus(1, 14, "PURGED")
	assertStatus(1, 13, "PURGING")
}
