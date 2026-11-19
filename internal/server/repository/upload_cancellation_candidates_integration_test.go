package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListSingleUploadCancellationCandidatesIntegration(t *testing.T) {
	// 1. 固定独占连接的 MySQL 时钟，保证微秒边界可重复。
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	execute("SET timestamp = ?", now.Unix())
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := pool.ExecContext(cleanupContext, "SET timestamp = DEFAULT"); err != nil {
			t.Error("恢复独占测试连接时钟失败", err)
		}
	})
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil || !databaseTime.Equal(now) {
		t.Fatal("独占测试连接的数据库时间不符", databaseTime, err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.ListSingleUploadCancellationCandidates(ctx, 100); candidates == nil || len(candidates) != 0 || err != nil {
		t.Fatal("无待取消任务时应返回空批次", candidates, err)
	}

	// 2. 混合用户、阶段、方式和等待条件，按乱序写入任务。
	past, future := now.Add(-time.Microsecond), now.Add(time.Microsecond)
	for _, row := range []struct {
		id, user, version uint64
		status, mode      string
		expiry            time.Time
		leaseAt, retryAt  *time.Time
	}{
		{id: 104, user: 7, version: 2, status: "CANCELLING", mode: "SINGLE", expiry: past, leaseAt: &past, retryAt: &past},
		{id: 100, user: 7, version: 3, status: "CANCELLING", mode: "SINGLE", expiry: past, leaseAt: &future},
		{id: 99, user: 7, status: "CANCELLING", mode: "MULTIPART", expiry: past},
		{id: 105, user: 7, status: "EXPIRING", mode: "SINGLE", expiry: past},
		{id: 103, user: 7, version: 9, status: "CANCELLING", mode: "SINGLE", expiry: future},
		{id: 101, user: 7, status: "CANCELLING", mode: "SINGLE", expiry: past, retryAt: &future},
		{id: 102, user: 7, status: "CANCELLED", mode: "SINGLE", expiry: past},
		{id: 106, user: 7, status: "UPLOADING", mode: "SINGLE", expiry: future},
		{id: math.MaxUint64, user: 8, version: math.MaxUint64, status: "CANCELLING", mode: "SINGLE", expiry: past, leaseAt: &now, retryAt: &now},
	} {
		deviceID, parentID := uint64(11), uint64(13)
		if row.user == 8 {
			deviceID, parentID = 12, 14
		}
		var leaseOwner, partSize, partCount any
		if row.leaseAt != nil {
			leaseOwner = "cancellation-cleanup-fixture"
		}
		if row.mode == "MULTIPART" {
			partSize, partCount = 5242880, 1
		}
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
			VALUES(?,?,'local_minio','fixture-bucket',?,0)`, row.id, row.user, "cancel-scan-"+strconv.FormatUint(row.id, 10))
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			file_size,part_size,part_count,status,reserved_bytes,lease_owner,lease_until,lease_version,next_attempt_at,expires_at)
			VALUES(?,?,?,?,?,'cancel-scan.bin',?,0,?,?,?,0,?,?,?,?,?)`, row.id, row.user, deviceID, row.id, parentID,
			row.mode, partSize, partCount, row.status, leaseOwner, row.leaseAt, row.version, row.retryAt, row.expiry)
	}

	// 3. 核对过滤先于限量、固定排序和读取前后的任务事实。
	taskBefore, err := store.FindUserUploadTask(ctx, 7, 104)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.UploadCancellationCandidate{
		{TaskID: 103, UserID: 7, StagedObjectID: 103, LeaseVersion: 9},
		{TaskID: 104, UserID: 7, StagedObjectID: 104, LeaseVersion: 2},
		{TaskID: math.MaxUint64, UserID: 8, StagedObjectID: math.MaxUint64, LeaseVersion: math.MaxUint64},
	}
	for _, limit := range []int{1, 2, 100, 100} {
		candidates, err := store.ListSingleUploadCancellationCandidates(ctx, limit)
		if err != nil || !reflect.DeepEqual(candidates, want[:min(limit, len(want))]) {
			t.Fatal("待取消收尾任务的范围或顺序不符", limit, candidates, err)
		}
	}
	if taskAfter, err := store.FindUserUploadTask(ctx, 7, 104); err != nil || !reflect.DeepEqual(taskAfter, taskBefore) {
		t.Fatal("候选扫描改变了任务事实", taskAfter, err)
	}

	// 4. 检查限量、事务范围与取消的错误行为。
	for _, limit := range []int{0, 101} {
		if candidates, err := store.ListSingleUploadCancellationCandidates(ctx, limit); candidates != nil || err == nil {
			t.Fatal("接受了无效批量上限", limit, candidates, err)
		}
	}
	if err := store.WithLoginUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
		if candidates, err := txStore.ListSingleUploadCancellationCandidates(txContext, 1); candidates != nil || err == nil {
			t.Error("用户事务内允许了跨用户取消扫描", candidates, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if candidates, err := store.ListSingleUploadCancellationCandidates(canceled, 1); candidates != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("取消扫描没有保留上下文原因", candidates, err)
	}
}
