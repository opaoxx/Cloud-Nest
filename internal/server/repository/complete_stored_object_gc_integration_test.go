package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCompleteStoredObjectGCIntegration(t *testing.T) {
	// 1. 提交带版本的删除意图，并准备缺少计时与版本的历史记录。
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
			t.Fatal("准备删除结果夹具失败", err)
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
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=31")
	execute(`UPDATE stored_objects SET unreferenced_at=?,gc_after=?,
		storage_version_id='fixture-gc-version',last_error_code='STORAGE_RETRY' WHERE id=29`,
		stamp.Add(-8*24*time.Hour), stamp.Add(-24*time.Hour))
	execute("UPDATE stored_objects SET status='DELETING' WHERE id=24")
	candidates, err := store.ListStoredObjectGCCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 || candidates[0].ObjectID != 29 {
		t.Fatal("未取得到期对象", candidates, err)
	}
	versioned, err := store.ClaimStoredObjectGC(ctx, candidates[0])
	if err != nil || versioned == nil {
		t.Fatal("未提交删除意图", versioned, err)
	}
	read := func(userID, objectID uint64) *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, userID, objectID)
		if err != nil {
			t.Fatal("读取删除结果失败", err)
		}
		return object
	}
	legacy := read(8, 24)
	originalTask, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 拒绝位置、版本或状态不符的删除记录。
	for _, scenario := range []struct {
		change func(*domain.StoredObject)
		want   error
	}{
		{func(row *domain.StoredObject) { row.ObjectKey += "-other" }, domain.ErrStoredObjectGCCandidateStale},
		{func(row *domain.StoredObject) { row.StorageVersionID = nil }, domain.ErrStoredObjectGCCandidateStale},
		{func(row *domain.StoredObject) { row.StorageVersionID = new("different-version") }, domain.ErrStoredObjectGCCandidateStale},
		{func(row *domain.StoredObject) { row.OwnerUserID = legacy.OwnerUserID }, domain.ErrStoredObjectNotFound},
		{func(row *domain.StoredObject) { row.Status = domain.StoredObjectStatusReady }, domain.ErrInvalidStoredObjectData},
	} {
		mismatch := *versioned
		scenario.change(&mismatch)
		if object, err := store.CompleteStoredObjectGC(ctx, &mismatch); object != nil || !errors.Is(err, scenario.want) {
			t.Fatal("不匹配的删除记录被写回", object, err)
		}
	}
	explicitNull := *legacy
	explicitNull.StorageVersionID = new("null")
	if object, err := store.CompleteStoredObjectGC(ctx, &explicitNull); object != nil || !errors.Is(err, domain.ErrStoredObjectGCCandidateStale) {
		t.Fatal("显式 null 版本与未登记版本应分别匹配", object, err)
	}

	// 3. 模拟读回时发现存储位置发生变化，检查回滚。
	execute(`CREATE TRIGGER fixture_change_completed_gc_location BEFORE UPDATE ON stored_objects
		FOR EACH ROW SET NEW.storage_version_id = 'fixture-wrong-version'`)
	if object, err := store.CompleteStoredObjectGC(ctx, versioned); object != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("读回错误的存储版本后仍交付完成记录", object, err)
	}
	if after := read(versioned.OwnerUserID, versioned.ID); !reflect.DeepEqual(after, versioned) {
		t.Fatal("异常回写未还原删除意图", after)
	}
	execute("DROP TRIGGER fixture_change_completed_gc_location")

	// 4. 确认两种版本定位均可完成，重放同一请求会保留已经提交的时间。
	for _, deleting := range []*domain.StoredObject{versioned, legacy} {
		earliest := now()
		completed, err := store.CompleteStoredObjectGC(ctx, deleting)
		if err != nil || completed == nil {
			t.Fatal("删除结果写回失败", completed, err)
		}
		latest := now()
		expected := *deleting
		expected.Status, expected.LastErrorCode, expected.UpdatedAt = domain.StoredObjectStatusDeleted, nil, completed.UpdatedAt
		if completed.UpdatedAt.Before(earliest) || completed.UpdatedAt.After(latest) || !reflect.DeepEqual(completed, &expected) {
			t.Fatal("提交后的状态、时间或审计字段不符", completed)
		}
		again, err := store.CompleteStoredObjectGC(ctx, deleting)
		if err != nil || !reflect.DeepEqual(again, completed) || !reflect.DeepEqual(read(deleting.OwnerUserID, deleting.ID), completed) {
			t.Fatal("重试改变了已经提交的删除结果", again, err)
		}
	}
	if objects, err := store.ListDeletingStoredObjects(ctx, 100); err != nil || len(objects) != 0 {
		t.Fatal("已完成对象仍在恢复扫描中", objects, err)
	}
	task, err := store.FindUserUploadTask(ctx, originalTask.UserID, originalTask.ID)
	if err != nil || !reflect.DeepEqual(task, originalTask) {
		t.Fatal("删除结果写回改变了上传审计记录", task, err)
	}
}
