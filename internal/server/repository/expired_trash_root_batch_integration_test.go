package repository

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestExpiredTrashRootBatchIntegration(t *testing.T) {
	// 1. 准备四条同刻到期的候选。第一项的版本已耗尽，第四项超出本轮上限。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	execute := func(sqlContext context.Context, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(sqlContext, statement, args...); err != nil {
			t.Fatal("准备到期批次夹具失败", err)
		}
	}
	var now time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
		t.Fatal(err)
	}
	deletedAt := now.AddDate(0, 0, -31)
	execute(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=? WHERE user_id=1 AND id=16", deletedAt)
	execute(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision) VALUES
		(17,1,11,'扫描后恢复',CAST('扫描后恢复' AS BINARY),'DIR','TRASHED',?,2),
		(18,1,11,'本轮标记',CAST('本轮标记' AS BINARY),'DIR','TRASHED',?,3),
		(20,1,11,'留到下一轮',CAST('留到下一轮' AS BINARY),'DIR','TRASHED',?,4)`, deletedAt, deletedAt, deletedAt)

	// 2. 用真实扫描与真实条件标记跑一轮，在扫描后模拟第二项被恢复。
	scans := 0
	batch := service.NewExpiredTrashRootBatch(func(scanContext context.Context, limit int) ([]domain.ExpiredTrashRootCandidate, error) {
		scans++
		candidates, err := store.ListExpiredTrashRootCandidates(scanContext, limit)
		if err != nil {
			return nil, err
		}
		var ids []uint64
		for _, candidate := range candidates {
			ids = append(ids, candidate.EntryID)
		}
		if !slices.Equal(ids, []uint64{16, 17, 18}) {
			t.Fatal("到期扫描顺序或上限不符", ids)
		}
		execute(scanContext, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,revision=3 WHERE user_id=1 AND id=17")
		return candidates, nil
	}, store.MarkExpiredTrashRootPurging)
	result, err := batch.RunOnce(ctx, 3)
	if err != nil || scans != 1 || result.Scanned != 3 || result.Marked != 1 || result.StaleCandidates != 1 || len(result.CandidateErrors) != 1 {
		t.Fatal("混合批次汇总不符", result, err)
	}
	failure := result.CandidateErrors[0]
	if failure.RootID != 16 || failure.UserID != 1 || !errors.Is(failure.Err, domain.ErrEntryRevisionExhausted) {
		t.Fatal("第一项的失败原因未保留", failure)
	}

	// 3. 确认第三项在前两项之后仍能提交，其他项保留各自的状态。
	for _, expected := range []struct {
		id     uint64
		status string
	}{{16, "TRASHED"}, {17, "ACTIVE"}, {18, "PURGING"}, {20, "TRASHED"}} {
		var row models.FileEntryTrash
		if err := store.database.WithContext(ctx).Where("user_id = ? AND id = ?", 1, expected.id).Take(&row).Error; err != nil {
			t.Fatal("读回批次后的夹具失败", expected.id, err)
		}
		if row.Status != expected.status || (row.PurgeStartedAt != nil) != (expected.status == "PURGING") {
			t.Fatal("条目状态或清理开始时间不符", expected.id, row.Status, row.PurgeStartedAt)
		}
	}
	var sequence, events, matchedEvents uint64
	if err := pool.QueryRowContext(ctx, `SELECT change_seq,
		(SELECT COUNT(*) FROM change_events WHERE user_id=users.id),
		(SELECT COUNT(*) FROM change_events AS events WHERE user_id=users.id AND events.sequence=1
			AND entry_id=18 AND revision=4 AND event_type='ENTRY_PURGE_STARTED') FROM users WHERE id=1`).
		Scan(&sequence, &events, &matchedEvents); err != nil || sequence != 1 || events != 1 || matchedEvents != 1 {
		t.Fatal("批次只有成功标记的根项应追加事件", sequence, events, matchedEvents, err)
	}
	t.Log("首项失败、第二项失效，第三项标记并记录序号 1，第四项留在下一轮")
}
