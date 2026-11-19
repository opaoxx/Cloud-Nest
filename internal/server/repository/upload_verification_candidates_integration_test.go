package repository

import (
	"context"
	"math"
	"reflect"
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
	future := now.Add(time.Microsecond)
	for _, fixture := range []struct {
		id, user, version uint64
		status, mode      string
		expires           time.Time
		retryAt, leaseAt  *time.Time
	}{
		{130, 7, 0, "VERIFYING", "SINGLE", expiry, nil, nil},
		{105, 8, 4, "VERIFYING", "SINGLE", expiry, &now, &now},
		{103, 7, 3, "VERIFYING", "SINGLE", expiry, nil, &future},
		{109, 7, 3, "VERIFYING", "SINGLE", expiry, &future, nil},
		{110, 7, 3, "VERIFYING", "SINGLE", now, nil, nil},
		{111, 7, 3, "UPLOADING", "SINGLE", expiry, nil, nil},
		{112, 7, 3, "VERIFYING", "MULTIPART", expiry, nil, nil},
		{113, 7, math.MaxUint64, "VERIFYING", "SINGLE", expiry, nil, nil},
		{math.MaxUint64, 8, math.MaxUint64 - 1, "VERIFYING", "SINGLE", expiry, nil, nil},
	} {
		deviceID, parentID := uint64(11), uint64(13)
		if fixture.user == 8 {
			deviceID, parentID = 12, 14
		}
		var leaseOwner, partSize, partCount any
		if fixture.leaseAt != nil {
			leaseOwner = "fixture-worker"
		}
		if fixture.mode == "MULTIPART" {
			partSize, partCount = 5242880, 1
		}
		execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status) VALUES(?,?,'local_minio','fixture-bucket',?,0,'VERIFYING')",
			fixture.id, fixture.user, "verification-fixture-"+strconv.FormatUint(fixture.id, 10))
		execute("INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,part_size,part_count,status,reserved_bytes,lease_owner,lease_until,lease_version,next_attempt_at,expires_at) VALUES(?,?,?,?,?,'verification.bin',?,0,?,?,?,0,?,?,?,?,?)",
			fixture.id, fixture.user, deviceID, fixture.id, parentID, fixture.mode, partSize, partCount, fixture.status,
			leaseOwner, fixture.leaseAt, fixture.version, fixture.retryAt, fixture.expires)
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
	execute("SET timestamp = ?", expiry.Unix())
	got, err := store.ListUploadVerificationCandidates(ctx, 100)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatal("任务恰好到期仍出现在真实候选中", got, err)
	}
}
