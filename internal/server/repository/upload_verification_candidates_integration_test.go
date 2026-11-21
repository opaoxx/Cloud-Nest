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

func TestListUploadVerificationCandidatesIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 1. 固定独占连接的 MySQL 时间，精确测试等于和相差一微秒。
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	execute("SET timestamp = ?", now.Unix())
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := pool.ExecContext(cleanupContext, "SET timestamp = DEFAULT"); err != nil {
			t.Error("恢复独占测试连接的时钟失败", err)
		}
	})
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil || !databaseTime.Equal(now) {
		t.Fatal("独占连接未使用固定数据库时间", databaseTime, err)
	}

	// 2. 混合两个用户、两种上传方式及未到期/刚到期边界。
	expiry := now.Add(time.Hour)
	past, future := now.Add(-time.Microsecond), now.Add(time.Microsecond)
	type fixture struct {
		id, user, version uint64
		status, mode      string
		expires           time.Time
		retryAt, leaseAt  *time.Time
		session           any
	}
	insert := func(fixture fixture) {
		t.Helper()
		deviceID, parentID := uint64(11), uint64(13)
		if fixture.user == 8 {
			deviceID, parentID = 12, 14
		}
		var leaseOwner, partSize, partCount any
		var size uint64
		if fixture.leaseAt != nil {
			leaseOwner = "fixture-worker"
		}
		if fixture.mode == "MULTIPART" {
			size = domain.MaxSingleUploadSizeBytes + 1
			partSize, partCount = domain.MaxSingleUploadSizeBytes, 2
		}
		execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status) VALUES(?,?,'local_minio','fixture-bucket',?,?,'VERIFYING')",
			fixture.id, fixture.user, "verification-fixture-"+strconv.FormatUint(fixture.id, 10), size)
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			storage_upload_id,expected_sha256,file_size,part_size,part_count,status,reserved_bytes,lease_owner,lease_until,lease_version,next_attempt_at,expires_at)
			VALUES(?,?,?,?,?,'verification.bin',?,?,UNHEX(REPEAT('ab',32)),?,?,?,?,?,?,?,?,?,?)`,
			fixture.id, fixture.user, deviceID, fixture.id, parentID, fixture.mode, fixture.session,
			size, partSize, partCount, fixture.status, size, leaseOwner, fixture.leaseAt, fixture.version, fixture.retryAt, fixture.expires)
	}
	const session = "fixture-verification-session"
	for _, task := range []fixture{
		{id: 130, user: 7, status: "VERIFYING", mode: "SINGLE", expires: expiry},
		{id: 105, user: 8, version: 4, status: "VERIFYING", mode: "SINGLE", expires: expiry, retryAt: &now, leaseAt: &now},
		{id: 103, user: 7, version: 3, status: "VERIFYING", mode: "SINGLE", expires: expiry, leaseAt: &future},
		{id: 109, user: 7, version: 3, status: "VERIFYING", mode: "SINGLE", expires: expiry, retryAt: &future},
		{id: 110, user: 7, version: 3, status: "VERIFYING", mode: "SINGLE", expires: now},
		{id: 111, user: 7, version: 3, status: "UPLOADING", mode: "SINGLE", expires: expiry},
		{id: 113, user: 7, version: math.MaxUint64, status: "VERIFYING", mode: "SINGLE", expires: expiry},
		{id: math.MaxUint64, user: 8, version: math.MaxUint64 - 1, status: "VERIFYING", mode: "SINGLE", expires: expiry},
		{id: 132, user: 7, status: "VERIFYING", mode: "MULTIPART", expires: expiry, session: session},
		{id: 106, user: 8, version: 4, status: "VERIFYING", mode: "MULTIPART", expires: expiry, retryAt: &now, leaseAt: &now, session: session},
		{id: 108, user: 7, version: math.MaxUint64 - 1, status: "VERIFYING", mode: "MULTIPART", expires: future, session: session},
		{id: 95, user: 7, status: "VERIFYING", mode: "MULTIPART", expires: expiry, leaseAt: &future, session: session},
		{id: 96, user: 7, status: "VERIFYING", mode: "MULTIPART", expires: expiry, retryAt: &future, session: session},
		{id: 97, user: 7, status: "VERIFYING", mode: "MULTIPART", expires: now, session: session},
		{id: 98, user: 7, status: "VERIFYING", mode: "MULTIPART", expires: past, session: session},
		{id: 99, user: 7, status: "COMPLETING", mode: "MULTIPART", expires: expiry, session: session},
		{id: 100, user: 7, status: "VERIFYING", mode: "MULTIPART", expires: expiry, session: ""},
		{id: 101, user: 7, status: "VERIFYING", mode: "MULTIPART", expires: expiry, session: "   "},
		{id: 102, user: 7, version: math.MaxUint64, status: "VERIFYING", mode: "MULTIPART", expires: expiry, session: session},
		{id: 112, user: 7, version: 3, status: "VERIFYING", mode: "MULTIPART", expires: expiry},
		{id: math.MaxUint64 - 1, user: 8, status: "VERIFYING", mode: "MULTIPART", expires: expiry, retryAt: &past, leaseAt: &past, session: session},
	} {
		insert(task)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	// 3. 验证排序、批量截断和重复扫描不改变租约。
	want := []domain.UploadVerificationCandidate{
		{TaskID: 105, UserID: 8, StagedObjectID: 105, LeaseVersion: 4},
		{TaskID: 130, UserID: 7, StagedObjectID: 130, LeaseVersion: 0},
		{TaskID: math.MaxUint64, UserID: 8, StagedObjectID: math.MaxUint64, LeaseVersion: math.MaxUint64 - 1},
	}
	before, err := store.FindUserUploadTask(ctx, 8, 105)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 2, 100, 100} {
		got, err := store.ListUploadVerificationCandidates(ctx, limit)
		if err != nil || !reflect.DeepEqual(got, want[:min(limit, len(want))]) {
			t.Fatal("真实数据库候选筛选或限制错误", limit, got, err)
		}
	}
	after, err := store.FindUserUploadTask(ctx, 8, 105)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("扫描改变了原任务记录", err)
	}

	// 4. 校验 Multipart 入选边界及 100 条上限，复查候选原有租约。
	wantMultipart := []domain.UploadVerificationCandidate{
		{TaskID: 106, UserID: 8, StagedObjectID: 106, LeaseVersion: 4},
		{TaskID: 108, UserID: 7, StagedObjectID: 108, LeaseVersion: math.MaxUint64 - 1},
		{TaskID: 132, UserID: 7, StagedObjectID: 132},
		{TaskID: math.MaxUint64 - 1, UserID: 8, StagedObjectID: math.MaxUint64 - 1},
	}
	if got, err := store.ListMultipartUploadVerificationCandidates(ctx, 100); err != nil || !slices.Equal(got, wantMultipart) {
		t.Fatal("Multipart 日期边界、模式、状态或会话筛选不符", got, err)
	}
	multipartBefore, err := store.FindUserUploadTask(ctx, 8, 106)
	if err != nil {
		t.Fatal(err)
	}
	firstBatch := slices.Clone(wantMultipart[:3])
	for id := uint64(200); id < 300; id++ {
		insert(fixture{id: id, user: 7, status: "VERIFYING", mode: "MULTIPART", expires: expiry, session: session})
		firstBatch = append(firstBatch, domain.UploadVerificationCandidate{TaskID: id, UserID: 7, StagedObjectID: id})
	}
	for _, limit := range []int{1, 3, 100, 100} {
		if got, err := store.ListMultipartUploadVerificationCandidates(ctx, limit); err != nil || !slices.Equal(got, firstBatch[:limit]) {
			t.Fatalf("Multipart 批量或顺序不符，limit=%d，候选=%d，错误=%v", limit, len(got), err)
		}
	}
	multipartAfter, err := store.FindUserUploadTask(ctx, 8, 106)
	if err != nil || !reflect.DeepEqual(multipartBefore, multipartAfter) {
		t.Fatal("Multipart 扫描改变了原任务或租约", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if got, err := store.ListMultipartUploadVerificationCandidates(canceled, 1); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("Multipart 扫描未遵循取消", got, err)
	}

	execute("SET timestamp = ?", expiry.Unix())
	got, err := store.ListUploadVerificationCandidates(ctx, 100)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatal("任务恰好到期仍出现在真实候选中", got, err)
	}
	if got, err := store.ListMultipartUploadVerificationCandidates(ctx, 100); err != nil || got == nil || len(got) != 0 {
		t.Fatal("恰好到期的 Multipart 仍进入候选", got, err)
	}
	t.Log("Multipart 首批任务 106、108、132、18446744073709551614，补齐后最多 100 条，恰好到期后 0 条")
}
