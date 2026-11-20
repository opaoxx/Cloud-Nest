package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestClaimStoredObjectGCIntegration(t *testing.T) {
	// 1. 准备已到期的 READY 和 ORPHANED 对象，保留可选存储版本与上传审计记录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, _ := uploadTaskDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备对象删除意图夹具失败", err)
		}
	}
	now := func() time.Time {
		t.Helper()
		var stamp time.Time
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&stamp); err != nil {
			t.Fatal(err)
		}
		return stamp
	}
	stamp := now()
	execute("UPDATE stored_objects SET status='ORPHANED',last_error_code='STORAGE_RETRY' WHERE id=23")
	execute(`UPDATE stored_objects SET storage_version_id='fixture-existing-version',
		unreferenced_at=?,gc_after=? WHERE id IN (23,29)`, stamp.Add(-8*24*time.Hour), stamp.Add(-24*time.Hour))
	rows, err := store.ListStoredObjectGCCandidates(ctx, 2)
	if err != nil || len(rows) != 2 || rows[0].ObjectID != 23 || rows[1].ObjectID != 29 {
		t.Fatal("未扫描到预期回收候选", rows, err)
	}
	orphaned, ready := rows[0], rows[1]
	read := func(candidate domain.StoredObjectGCCandidate) *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, candidate.OwnerUserID, candidate.ObjectID)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	before := read(ready)

	// 2. 有效文件引用会挡住领取，读回位置异常时整笔更新回滚。
	if object, err := store.ClaimStoredObjectGC(ctx, ready); object != nil || !errors.Is(err, domain.ErrStoredObjectInUse) {
		t.Fatal("仍有有效引用的对象被领取", object, err)
	}
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=31")
	execute(`CREATE TRIGGER fixture_change_gc_location BEFORE UPDATE ON stored_objects
		FOR EACH ROW SET NEW.storage_version_id = 'fixture-wrong-version'`)
	if object, err := store.ClaimStoredObjectGC(ctx, ready); object != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("读回错误的存储版本后仍交付对象", object, err)
	}
	if after := read(ready); !reflect.DeepEqual(after, before) {
		t.Fatal("读回错误后状态或位置未回滚", after)
	}
	execute("DROP TRIGGER fixture_change_gc_location")

	// 3. 两个执行者竞争同一候选，仅一方得到已提交的删除意图。
	for _, candidate := range []domain.StoredObjectGCCandidate{ready, orphaned} {
		before := read(candidate)
		earliest := now()
		type claimResult struct {
			object *domain.StoredObject
			err    error
		}
		results := make(chan claimResult, 2)
		for range 2 {
			go func() {
				object, err := store.ClaimStoredObjectGC(ctx, candidate)
				results <- claimResult{object, err}
			}()
		}
		var claimed *domain.StoredObject
		stale := 0
		for range 2 {
			result := <-results
			switch {
			case result.err == nil && result.object != nil && claimed == nil:
				claimed = result.object
			case errors.Is(result.err, domain.ErrStoredObjectGCCandidateStale) && result.object == nil:
				stale++
			default:
				t.Error("并发领取返回异常结果", candidate.ObjectID, result.object, result.err)
			}
		}
		if claimed == nil || stale != 1 {
			t.Fatal("同一对象应只有一次领取成功", candidate.ObjectID, stale, claimed)
		}
		latest := now()
		expected := *before
		expected.Status, expected.DedupHash, expected.UpdatedAt = domain.StoredObjectStatusDeleting, nil, claimed.UpdatedAt
		if claimed.UpdatedAt.Before(earliest) || claimed.UpdatedAt.After(latest) ||
			!reflect.DeepEqual(claimed, &expected) || !reflect.DeepEqual(read(candidate), claimed) {
			t.Fatal("已提交的删除意图、更新时间或对象事实不符", claimed)
		}
	}
	if remaining, err := store.ListStoredObjectGCCandidates(ctx, 100); err != nil || len(remaining) != 0 {
		t.Fatal("已领取对象仍出现在到期候选扫描中", remaining, err)
	}
}
