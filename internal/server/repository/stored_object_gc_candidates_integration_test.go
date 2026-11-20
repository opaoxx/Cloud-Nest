package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListStoredObjectGCCandidatesIntegration(t *testing.T) {
	// 1. 在独占库准备跨用户的对象，以及数据库当前时间和微秒相邻的期限。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListStoredObjectGCCandidates(ctx, 100); err != nil || rows == nil || len(rows) != 0 {
		t.Fatal("没有到期对象时应返回空批次", rows, err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备对象回收候选夹具失败", err)
		}
	}
	execute(`INSERT INTO users(id,username,display_name,password_hash,status)
		VALUES (1,'gc_owner','回收对象用户','fixture_hash','ACTIVE'),(2,'gc_disabled','禁用对象用户','fixture_hash','DISABLED')`)
	var now time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
		t.Fatal(err)
	}
	ready, orphaned := domain.StoredObjectStatusReady, domain.StoredObjectStatusOrphaned
	const gracePeriod = 7 * 24 * time.Hour
	earliest := now.Add(-time.Microsecond)
	want := []domain.StoredObjectGCCandidate{
		{ObjectID: 81, OwnerUserID: 2, Status: orphaned, UnreferencedAt: earliest.Add(-gracePeriod), GCAfter: earliest},
		{ObjectID: 71, OwnerUserID: 1, Status: ready, UnreferencedAt: now.Add(-gracePeriod), GCAfter: now},
		{ObjectID: 82, OwnerUserID: 2, Status: orphaned, UnreferencedAt: now.Add(-gracePeriod), GCAfter: now},
		{ObjectID: math.MaxUint64, OwnerUserID: 1, Status: ready, UnreferencedAt: now.Add(-8 * 24 * time.Hour), GCAfter: now},
	}
	insert := func(objectID, ownerID uint64, status domain.StoredObjectStatus, startedAt, gcAfter any) {
		key := fmt.Sprintf("gc-candidate-%d", objectID)
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,
			verified_sha256,verified_at,status,unreferenced_at,gc_after)
			VALUES(?,?,'MINIO','fixture-bucket',?,2048,UNHEX(SHA2(?,256)),?,?,?,?)`,
			objectID, ownerID, key, key, now.Add(-10*24*time.Hour), status, startedAt, gcAfter)
	}
	insert(61, 1, ready, now.Add(time.Hour-gracePeriod), now.Add(time.Hour))
	insert(62, 1, orphaned, nil, nil)
	insert(63, 1, ready, earliest.Add(-gracePeriod+time.Microsecond), earliest)
	for index, status := range []domain.StoredObjectStatus{
		domain.StoredObjectStatusStaging, domain.StoredObjectStatusVerifying, domain.StoredObjectStatusDeleting, domain.StoredObjectStatusDeleted,
	} {
		insert(64+uint64(index), 1, status, earliest.Add(-gracePeriod), earliest)
	}
	for _, candidate := range want {
		insert(candidate.ObjectID, candidate.OwnerUserID, candidate.Status, candidate.UnreferencedAt, candidate.GCAfter)
	}

	// 2. 验证两种状态、完整宽限期、双字段排序和数量上限。
	for _, limit := range []int{1, 2, 100, 100} {
		rows, err := store.ListStoredObjectGCCandidates(ctx, limit)
		if err != nil || !reflect.DeepEqual(rows, want[:min(limit, len(want))]) {
			t.Fatal("对象回收候选的字段、顺序或限量不符", limit, rows, err)
		}
	}
	for _, limit := range []int{0, 101} {
		if rows, err := store.ListStoredObjectGCCandidates(ctx, limit); rows != nil || err == nil {
			t.Fatal("接受了越界批量大小", limit, rows, err)
		}
	}
	err = store.withActiveUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
		if rows, err := txStore.ListStoredObjectGCCandidates(txContext, 1); rows != nil || err == nil {
			t.Error("用户事务不应发起跨用户扫描", rows, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if rows, err := store.ListStoredObjectGCCandidates(canceled, 1); rows != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("扫描未保留取消原因", rows, err)
	}

	// 3. 确认扫描只读已提交的状态，提交删除意图后候选离开本批。
	transaction, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, "UPDATE stored_objects SET status='DELETING' WHERE id=81"); err != nil {
		t.Fatal(err)
	}
	readContext, release := context.WithTimeout(ctx, 2*time.Second)
	defer release()
	if rows, err := store.ListStoredObjectGCCandidates(readContext, 1); err != nil || !reflect.DeepEqual(rows, want[:1]) {
		t.Fatal("并发未提交时应读取原候选", rows, err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if rows, err := store.ListStoredObjectGCCandidates(ctx, 100); err != nil || !reflect.DeepEqual(rows, want[1:]) {
		t.Fatal("已提交删除意图的对象应从到期候选消失", rows, err)
	}
}
