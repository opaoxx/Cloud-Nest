package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListPurgeLeafCandidatesIntegration(t *testing.T) {
	// 1. 准备清理根项、不同状态的后代及已完成的墓碑。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备叶子候选夹具失败", err)
		}
	}
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	finishedAt := startedAt.Add(time.Second)
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=?,revision=revision+1 WHERE id=14", startedAt)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at)
		VALUES (17,1,14,'待清理回收目录',CAST('待清理回收目录' AS BINARY),'DIR','TRASHED','2026-01-01',NULL,NULL),
		       (18,1,17,'已清理后代',CAST('已清理后代' AS BINARY),'DIR','PURGED',NULL,NULL,?),
		       (19,1,17,'活动后代',CAST('活动后代' AS BINARY),'DIR','ACTIVE',NULL,NULL,NULL),
		       (22,1,17,'独立受理后代',CAST('独立受理后代' AS BINARY),'DIR','PURGING',NULL,?,NULL)`,
		finishedAt, startedAt.Add(-time.Microsecond))

	roots, err := store.ListPurgeRootCandidates(ctx, 10)
	if err != nil || len(roots) != 2 || roots[1].EntryID != 14 {
		t.Fatal("未读取到预期清理根项", roots, err)
	}
	root := roots[1]
	read := func(candidate domain.PurgeRootCandidate, limit int) ([]domain.PurgeLeafCandidate, error) {
		var leaves []domain.PurgeLeafCandidate
		err := store.WithPurgeRootTransaction(ctx, candidate, func(txContext context.Context, txStore *Store) error {
			var readErr error
			leaves, readErr = txStore.ListPurgeLeafCandidates(txContext, limit)
			return readErr
		})
		return leaves, err
	}
	check := func(expected []domain.PurgeLeafCandidate, limit int) {
		t.Helper()
		actual, err := read(root, limit)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("叶子候选实际 %+v，预期 %+v，错误 %v", actual, expected, err)
		}
	}

	// 2. 只有没有待清理子项的项目入选，按 ID 升序截取。
	want := []domain.PurgeLeafCandidate{
		{EntryID: 15, ParentID: 14, EntryType: "FILE", Status: "ACTIVE", Revision: 5, FileSize: 2048},
		{EntryID: 19, ParentID: 17, EntryType: "DIR", Status: "ACTIVE", Revision: 1},
		{EntryID: 22, ParentID: 17, EntryType: "DIR", Status: "PURGING", Revision: 1},
	}
	check(want[:1], 1)
	check(want, 100)
	check(want, 100)
	for _, limit := range []int{0, 101} {
		if result, err := read(root, limit); result != nil || err == nil {
			t.Fatal("接受了非法批量大小", result, err)
		}
	}
	if result, err := store.ListPurgeLeafCandidates(ctx, 1); result != nil || err == nil {
		t.Fatal("普通 Store 不应读取根项内的叶子", result, err)
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		if result, err := txStore.ListPurgeLeafCandidates(txContext, 1); result != nil || err == nil {
			t.Error("未绑定清理根项的用户事务不应读取叶子", result, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 3. 模拟子项依次完成，确认墓碑不阻塞父目录，根项最后入选。
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=? WHERE id IN (15,19,22)", finishedAt)
	check([]domain.PurgeLeafCandidate{{EntryID: 17, ParentID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 1}}, 100)
	execute("UPDATE file_entries SET status='PURGED',purged_at=? WHERE id=17", finishedAt)
	check([]domain.PurgeLeafCandidate{{EntryID: 14, ParentID: 11, EntryType: "DIR", Status: "PURGING", Revision: 2}}, 100)

	// 4. 完成的父目录下若仍有待清理子项，整批拒绝。
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES(24,1,18,'异常后代',CAST('异常后代' AS BINARY),'DIR')`)
	if result, err := read(root, 1); result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("异常结构不应返回部分叶子", result, err)
	}
}
