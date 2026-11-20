package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListDeletingStoredObjectsIntegration(t *testing.T) {
	// 1. 领取一个到期对象，另准备跨用户及缺少计时的已提交删除记录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, _ := uploadTaskDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if objects, err := store.ListDeletingStoredObjects(ctx, 100); err != nil || objects == nil || len(objects) != 0 {
		t.Fatal("没有删除记录时应返回空批次", objects, err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备删除恢复夹具失败", err)
		}
	}
	var now time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
		t.Fatal(err)
	}
	earliest := now.Add(-24 * time.Hour)
	later := earliest.Add(time.Microsecond)
	const gracePeriod = 7 * 24 * time.Hour
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=31")
	execute(`UPDATE stored_objects SET unreferenced_at=?,gc_after=?,storage_version_id='fixture-claim-version'
		WHERE id=29`, earliest.Add(-gracePeriod), earliest)
	execute("UPDATE stored_objects SET status='DELETING' WHERE id=23")
	execute("UPDATE stored_objects SET status='DELETING',unreferenced_at=?,gc_after=? WHERE id=24",
		later.Add(-gracePeriod), later)
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,unreferenced_at,gc_after)
		VALUES (25,8,'MINIO','fixture-bucket','gc-retry-tie',0,'DELETING',?,?),
		       (26,8,'MINIO','fixture-bucket','gc-still-orphaned',0,'ORPHANED',?,?)`,
		later.Add(-gracePeriod), later, earliest.Add(-gracePeriod), earliest)
	candidates, err := store.ListStoredObjectGCCandidates(ctx, 2)
	if err != nil || len(candidates) != 2 || candidates[1].ObjectID != 29 {
		t.Fatal("未取得待领取的 READY 对象", candidates, err)
	}
	claimed, err := store.ClaimStoredObjectGC(ctx, candidates[1])
	if err != nil || claimed == nil || claimed.StorageVersionID == nil || *claimed.StorageVersionID != "fixture-claim-version" {
		t.Fatal("未提交带版本定位的删除意图", claimed, err)
	}

	// 2. 新仓储实例只用数据库恢复原记录，核对排序、上限和版本定位。
	restarted, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	var want []domain.StoredObject
	for _, target := range []struct{ userID, objectID uint64 }{{7, 23}, {7, 29}, {8, 24}, {8, 25}} {
		object, err := restarted.FindUserStoredObject(ctx, target.userID, target.objectID)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, *object)
	}
	if want[0].GCAfter != nil || want[0].StorageVersionID != nil || !reflect.DeepEqual(want[1], *claimed) {
		t.Fatal("恢复夹具中的历史记录或已提交版本不符", want)
	}
	for _, limit := range []int{1, 2, 100, 100} {
		objects, err := restarted.ListDeletingStoredObjects(ctx, limit)
		if err != nil || !reflect.DeepEqual(objects, want[:min(limit, len(want))]) {
			t.Fatal("删除恢复记录、顺序或限量不符", limit, objects, err)
		}
	}
	for _, limit := range []int{0, 101} {
		if objects, err := restarted.ListDeletingStoredObjects(ctx, limit); objects != nil || err == nil {
			t.Fatal("接受了越界批量大小", limit, objects, err)
		}
	}
	if err := restarted.withActiveUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
		if objects, err := txStore.ListDeletingStoredObjects(txContext, 1); objects != nil || err == nil {
			t.Error("用户事务内不应执行跨用户恢复扫描", objects, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if objects, err := restarted.ListDeletingStoredObjects(canceled, 1); objects != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("恢复扫描未保留取消原因", objects, err)
	}

	// 3. 错误的位置被拒绝，已完成的删除在提交后移出恢复批次。
	execute("UPDATE stored_objects SET provider='' WHERE id=23")
	if objects, err := restarted.ListDeletingStoredObjects(ctx, 100); objects != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("无效存储位置通过了恢复扫描", objects, err)
	}
	execute("UPDATE stored_objects SET provider='MINIO' WHERE id=23")
	transaction, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, "UPDATE stored_objects SET status='DELETED' WHERE id=24"); err != nil {
		t.Fatal(err)
	}
	readContext, release := context.WithTimeout(ctx, 2*time.Second)
	defer release()
	if objects, err := restarted.ListDeletingStoredObjects(readContext, 100); err != nil || !reflect.DeepEqual(objects, want) {
		t.Fatal("未提交的完成写入不应改变恢复扫描结果", objects, err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if objects, err := restarted.ListDeletingStoredObjects(ctx, 100); err != nil ||
		!reflect.DeepEqual(objects, []domain.StoredObject{want[0], want[1], want[3]}) {
		t.Fatal("删除完成提交后，恢复批次未正确移除对象", objects, err)
	}
}
