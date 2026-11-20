package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListPurgeRootCandidatesIntegration(t *testing.T) {
	// 1. 在独占库验证空批次，再写入不同状态及归属的测试目录项。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	if rows, err := store.ListPurgeRootCandidates(ctx, 100); rows == nil || len(rows) != 0 || err != nil {
		t.Fatal("无待清理根项时应返回空批次", rows, err)
	}

	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	earliest := startedAt.Add(-time.Microsecond)
	later := startedAt.Add(time.Microsecond)
	if _, err := pool.ExecContext(ctx, `UPDATE file_entries
		SET status='PURGING',purge_started_at=?,deleted_at='2026-01-01',revision=revision+1 WHERE user_id=1 AND id=13`, startedAt); err != nil {
		t.Fatal("准备清理文件夹具失败", err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries
		(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at,purged_at,revision)
		VALUES (17,1,11,'清理目录',CAST('清理目录' AS BINARY),'DIR','PURGING',?,NULL,2),
		       (22,1,17,'独立受理后代',CAST('独立受理后代' AS BINARY),'DIR','PURGING',?,NULL,3),
		       (18,2,21,'另一账号清理目录',CAST('另一账号清理目录' AS BINARY),'DIR','PURGING',?,NULL,4),
		       (24,1,11,'已清理目录',CAST('已清理目录' AS BINARY),'DIR','PURGED',?,?,6),
		       (?,2,21,'最大标识',CAST('最大标识' AS BINARY),'DIR','PURGING',?,NULL,?)`,
		startedAt, later, earliest, earliest, later, uint64(math.MaxUint64), later, uint64(math.MaxUint64)); err != nil {
		t.Fatal("准备清理根项夹具失败", err)
	}
	want := []domain.PurgeRootCandidate{
		{EntryID: 18, UserID: 2, ParentID: 21, Revision: 4, PurgeStartedAt: earliest},
		{EntryID: 13, UserID: 1, ParentID: 12, Revision: 4, PurgeStartedAt: startedAt},
		{EntryID: 17, UserID: 1, ParentID: 11, Revision: 2, PurgeStartedAt: startedAt},
		{EntryID: 22, UserID: 1, ParentID: 17, Revision: 3, PurgeStartedAt: later},
		{EntryID: math.MaxUint64, UserID: 2, ParentID: 21, Revision: math.MaxUint64, PurgeStartedAt: later},
	}

	// 2. 核对过滤、微秒时间顺序、同时间 ID 和限量，重复扫描结果一致。
	for _, limit := range []int{1, 2, 100, 100} {
		rows, err := store.ListPurgeRootCandidates(ctx, limit)
		if err != nil || !reflect.DeepEqual(rows, want[:min(limit, len(want))]) {
			t.Fatal("清理根项候选的范围、排序或字段错误", limit, rows, err)
		}
	}
	for _, limit := range []int{0, 101} {
		if rows, err := store.ListPurgeRootCandidates(ctx, limit); rows != nil || err == nil {
			t.Fatal("接受了无效批量", limit, rows, err)
		}
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		if rows, err := txStore.ListPurgeRootCandidates(txContext, 1); rows != nil || err == nil {
			t.Error("用户事务内允许跨用户扫描", rows, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if rows, err := store.ListPurgeRootCandidates(canceled, 1); rows != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("查询未保留取消原因", rows, err)
	}

	// 3. 用另一连接模拟未提交的完成写入，扫描仍读取最近已提交状态。
	transaction, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `UPDATE file_entries SET status='PURGED',purged_at=?
		WHERE user_id=2 AND id=18`, later); err != nil {
		t.Fatal(err)
	}
	readContext, release := context.WithTimeout(ctx, 2*time.Second)
	defer release()
	if rows, err := store.ListPurgeRootCandidates(readContext, 1); err != nil || !reflect.DeepEqual(rows, want[:1]) {
		t.Fatal("未提交写入期间应读到原候选", rows, err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListPurgeRootCandidates(ctx, 100); err != nil || !reflect.DeepEqual(rows, want[1:]) {
		t.Fatal("已完成项提交后应移出候选", rows, err)
	}
}
