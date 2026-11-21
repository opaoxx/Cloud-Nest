package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListMultipartUploadCompletionCandidatesIntegration(t *testing.T) {
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

	// 1. 固定独占数据库连接的时钟，准确覆盖到期的微秒边界。
	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
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
		t.Fatal("未能固定独占连接的数据库时钟", databaseTime, err)
	}
	execute("UPDATE users SET status='ACTIVE' WHERE id=8")

	// 2. 写入两个合成账号的任务，使符合和不符合条件的任务交错。
	expiry := now.Add(time.Hour)
	future := now.Add(time.Microsecond)
	type fixture struct {
		id, user, version uint64
		mode, status      string
		session           any
		expires           time.Time
		retryAt, leaseAt  *time.Time
	}
	insert := func(task fixture) {
		t.Helper()
		deviceID, parentID := uint64(11), uint64(13)
		if task.user == 8 {
			deviceID, parentID = 12, 14
		}
		var partSize, partCount, leaseOwner any
		size := uint64(1)
		if task.mode == "MULTIPART" {
			size = domain.MaxSingleUploadSizeBytes + 1
			partSize, partCount = domain.MaxSingleUploadSizeBytes, 2
		}
		if task.leaseAt != nil {
			leaseOwner = "fixture-merger"
		}
		execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'local_minio','fixture-bucket',?,?)",
			task.id, task.user, "completion-candidate-"+strconv.FormatUint(task.id, 10), size)
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			storage_upload_id,expected_sha256,file_size,part_size,part_count,status,reserved_bytes,lease_owner,lease_until,lease_version,next_attempt_at,expires_at)
			VALUES(?,?,?,?,?,'fixture.bin',?,?,UNHEX(REPEAT('ab',32)),?,?,?,?,?,?,?,?,?,?)`,
			task.id, task.user, deviceID, task.id, parentID, task.mode, task.session,
			size, partSize, partCount, task.status, size, leaseOwner, task.leaseAt, task.version, task.retryAt, task.expires)
	}
	const session = "fixture-bound-session"
	for _, task := range []fixture{
		{id: 130, user: 7, mode: "MULTIPART", status: "COMPLETING", session: session, expires: expiry},
		{id: 105, user: 8, version: 4, mode: "MULTIPART", status: "COMPLETING", session: session, expires: expiry, retryAt: &now, leaseAt: &now},
		{id: 108, user: 7, version: math.MaxUint64 - 1, mode: "MULTIPART", status: "COMPLETING", session: session, expires: future},
		{id: 100, user: 7, version: 3, mode: "MULTIPART", status: "COMPLETING", session: session, expires: expiry, leaseAt: &future},
		{id: 101, user: 7, mode: "MULTIPART", status: "COMPLETING", session: session, expires: expiry, retryAt: &future},
		{id: 102, user: 7, mode: "MULTIPART", status: "COMPLETING", session: session, expires: now},
		{id: 103, user: 7, mode: "MULTIPART", status: "UPLOADING", session: session, expires: expiry},
		{id: 104, user: 7, mode: "SINGLE", status: "COMPLETING", expires: expiry},
		{id: 106, user: 7, version: math.MaxUint64, mode: "MULTIPART", status: "COMPLETING", session: session, expires: expiry},
		{id: 107, user: 7, mode: "MULTIPART", status: "COMPLETING", expires: expiry},
		{id: 109, user: 7, mode: "MULTIPART", status: "COMPLETING", session: "", expires: expiry},
		{id: 110, user: 7, mode: "MULTIPART", status: "COMPLETING", session: "   ", expires: expiry},
	} {
		insert(task)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.MultipartUploadCompletionCandidate{
		{TaskID: 105, UserID: 8, StagedObjectID: 105, LeaseVersion: 4},
		{TaskID: 108, UserID: 7, StagedObjectID: 108, LeaseVersion: math.MaxUint64 - 1},
		{TaskID: 130, UserID: 7, StagedObjectID: 130},
	}
	before, err := store.FindUserUploadTask(ctx, 8, 105)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ListMultipartUploadCompletionCandidates(ctx, 100)
	if err != nil || !slices.Equal(got, want) {
		t.Fatal("数据库时间、会话或状态筛选不符", got, err)
	}

	// 3. 核对稳定排序、最大批次、重复扫描及只读保证。
	for id := uint64(200); id < 300; id++ {
		insert(fixture{id: id, user: 7, mode: "MULTIPART", status: "COMPLETING", session: session, expires: expiry})
		want = append(want, domain.MultipartUploadCompletionCandidate{TaskID: id, UserID: 7, StagedObjectID: id})
	}
	for _, limit := range []int{1, 3, 100, 100} {
		got, err := store.ListMultipartUploadCompletionCandidates(ctx, limit)
		if err != nil || !slices.Equal(got, want[:limit]) {
			t.Fatalf("扫描上限或稳定顺序不符：limit=%d，候选=%d，错误=%v", limit, len(got), err)
		}
	}
	after, err := store.FindUserUploadTask(ctx, 8, 105)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("读取候选改变了任务或已有租约", err)
	}
	for _, limit := range []int{0, 101} {
		if got, err := store.ListMultipartUploadCompletionCandidates(ctx, limit); got != nil || err == nil {
			t.Fatal("接受了无效批量上限", limit)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if got, err := store.ListMultipartUploadCompletionCandidates(canceled, 1); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("扫描未遵循取消", err)
	}
	execute("SET timestamp = ?", expiry.Unix())
	if got, err := store.ListMultipartUploadCompletionCandidates(ctx, 100); err != nil || got == nil || len(got) != 0 {
		t.Fatal("恰好到期的任务仍进入候选", got, err)
	}
	t.Log("未限量的首批候选任务为 105、108、130，单批最多 100 条，到期后 0 条")
}
