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

func TestListMultipartUploadInitializationCandidatesIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}

	// 1. 固定独占 MySQL 会话的时钟，验证微秒边界。
	now := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	execute("SET timestamp = ?", now.Unix())
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := pool.ExecContext(cleanupContext, "SET timestamp = DEFAULT"); err != nil {
			t.Error("恢复隔离连接时间失败", err)
		}
	})
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil || !databaseTime.Equal(now) {
		t.Fatal("独占连接未使用固定数据库时间", databaseTime, err)
	}

	// 2. 混合两个账号和不同任务状态，覆盖重试、租约、会话与到期条件。
	expiry := now.Add(time.Hour)
	future := now.Add(time.Microsecond)
	type taskFixture struct {
		id, user, version uint64
		mode, status      string
		storageSession    any
		expires           time.Time
		retryAt, leaseAt  *time.Time
	}
	insert := func(fixture taskFixture) {
		t.Helper()
		deviceID, parentID := uint64(11), uint64(13)
		if fixture.user == 8 {
			deviceID, parentID = 12, 14
		}
		var partSize, partCount, leaseOwner any
		size := uint64(1)
		if fixture.mode == "MULTIPART" {
			size = domain.MaxSingleUploadSizeBytes + 1
			partSize, partCount = domain.MaxSingleUploadSizeBytes, 2
		}
		if fixture.leaseAt != nil {
			leaseOwner = "fixture-worker"
		}
		execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'local_minio','fixture-bucket',?,?)",
			fixture.id, fixture.user, "initialization-fixture-"+strconv.FormatUint(fixture.id, 10), size)
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			storage_upload_id,expected_sha256,file_size,part_size,part_count,status,reserved_bytes,lease_owner,lease_until,lease_version,next_attempt_at,expires_at)
			VALUES(?,?,?,?,?,'fixture.bin',?,?,UNHEX(REPEAT('ab',32)),?,?,?,?,?,?,?,?,?,?)`,
			fixture.id, fixture.user, deviceID, fixture.id, parentID, fixture.mode, fixture.storageSession,
			size, partSize, partCount, fixture.status, size, leaseOwner, fixture.leaseAt, fixture.version, fixture.retryAt, fixture.expires)
	}
	for _, fixture := range []taskFixture{
		{id: 130, user: 7, mode: "MULTIPART", status: "CREATING", expires: expiry},
		{id: 105, user: 8, version: 4, mode: "MULTIPART", status: "CREATING", expires: expiry, retryAt: &now, leaseAt: &now},
		{id: 108, user: 7, mode: "MULTIPART", status: "CREATING", expires: future},
		{id: 100, user: 7, version: 3, mode: "MULTIPART", status: "CREATING", expires: expiry, leaseAt: &future},
		{id: 101, user: 7, mode: "MULTIPART", status: "CREATING", expires: expiry, retryAt: &future},
		{id: 102, user: 7, mode: "MULTIPART", status: "CREATING", expires: now},
		{id: 103, user: 7, mode: "MULTIPART", status: "UPLOADING", expires: expiry, storageSession: "fixture-existing"},
		{id: 104, user: 7, mode: "SINGLE", status: "CREATING", expires: expiry},
		{id: 106, user: 7, version: math.MaxUint64, mode: "MULTIPART", status: "CREATING", expires: expiry},
		{id: 107, user: 7, mode: "MULTIPART", status: "CREATING", expires: expiry, storageSession: "fixture-already-bound"},
	} {
		insert(fixture)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.MultipartUploadInitializationCandidate{
		{TaskID: 105, UserID: 8, StagedObjectID: 105, LeaseVersion: 4},
		{TaskID: 108, UserID: 7, StagedObjectID: 108},
		{TaskID: 130, UserID: 7, StagedObjectID: 130},
	}
	before, err := store.FindUserUploadTask(ctx, 8, 105)
	if err != nil {
		t.Fatal(err)
	}

	// 3. 核对排序、最大批量、重复扫描和只读性质。
	for taskID := uint64(200); taskID < 300; taskID++ {
		insert(taskFixture{id: taskID, user: 7, mode: "MULTIPART", status: "CREATING", expires: expiry})
		want = append(want, domain.MultipartUploadInitializationCandidate{TaskID: taskID, UserID: 7, StagedObjectID: taskID})
	}
	for _, limit := range []int{1, 3, 100, 100} {
		got, err := store.ListMultipartUploadInitializationCandidates(ctx, limit)
		if err != nil || !reflect.DeepEqual(got, want[:limit]) {
			t.Fatalf("候选筛选或顺序错误，limit=%d，候选数=%d，错误=%v", limit, len(got), err)
		}
	}
	after, err := store.FindUserUploadTask(ctx, 8, 105)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("读取候选改变了任务记录", err)
	}
	for _, invalidLimit := range []int{0, 101} {
		if got, err := store.ListMultipartUploadInitializationCandidates(ctx, invalidLimit); err == nil || got != nil {
			t.Fatalf("无效批量限制被接受: %d", invalidLimit)
		}
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if got, err := store.ListMultipartUploadInitializationCandidates(cancelled, 1); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("扫描没有遵循上下文取消: %v", err)
	}
	execute("SET timestamp = ?", expiry.Unix())
	if got, err := store.ListMultipartUploadInitializationCandidates(ctx, 100); err != nil || got == nil || len(got) != 0 {
		t.Fatal("恰好到期的任务仍进入候选", got, err)
	}
}
