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

func TestListUploadExpirationCandidatesIntegration(t *testing.T) {
	// 1. 固定独占测试连接的 MySQL 时间，准备可重复的微秒边界。
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
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
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
		t.Fatal("测试连接未使用预期的数据库时间", databaseTime, err)
	}
	past, earlier, future := now.Add(-time.Microsecond), now.Add(-2*time.Microsecond), now.Add(time.Microsecond)
	const multipartSize = (16 << 20) + 1
	execute("UPDATE users SET reserved_storage=? WHERE id=7", 9+multipartSize)

	// 2. 混合上传阶段、租约、重试时点和用户，按乱序写入。
	for _, fixture := range []struct {
		id, user, version uint64
		status, mode      string
		expiry            time.Time
		leaseAt, retryAt  *time.Time
		size              uint64
	}{
		{id: 100, user: 7, version: 3, status: "UPLOADING", mode: "SINGLE", expiry: now, retryAt: &future, size: 9},
		{id: 122, user: 7, version: 2, status: "EXPIRING", mode: "SINGLE", expiry: past, leaseAt: &now, retryAt: &now},
		{id: 105, user: 8, status: "CREATING", mode: "SINGLE", expiry: earlier.Add(-time.Microsecond)},
		{id: 150, user: 7, version: 5, status: "COMPLETING", mode: "MULTIPART", expiry: earlier, retryAt: &future, size: multipartSize},
		{id: 151, user: 8, version: 7, status: "EXPIRING", mode: "MULTIPART", expiry: past, leaseAt: &now, retryAt: &now},
		{id: 152, user: 7, status: "EXPIRING", mode: "MULTIPART", expiry: past, retryAt: &future},
		{id: 153, user: 7, status: "VERIFYING", mode: "MULTIPART", expiry: past, leaseAt: &future},
		{id: 154, user: 7, status: "UPLOADING", mode: "MULTIPART", expiry: future},
		{id: 155, user: 7, status: "CANCELLING", mode: "MULTIPART", expiry: past},
		{id: 156, user: 7, status: "CANCELLED", mode: "MULTIPART", expiry: past},
		{id: 157, user: 7, status: "CREATING", mode: "MULTIPART", expiry: now},
		{id: 108, user: 7, version: 3, status: "VERIFYING", mode: "SINGLE", expiry: past, leaseAt: &future},
		{id: 109, user: 7, version: 3, status: "EXPIRING", mode: "SINGLE", expiry: past, retryAt: &future},
		{id: 110, user: 7, status: "UPLOADING", mode: "SINGLE", expiry: future},
		{id: 111, user: 7, status: "CANCELLING", mode: "SINGLE", expiry: past},
		{id: 112, user: 7, status: "CANCELLED", mode: "SINGLE", expiry: past},
		{id: 113, user: 7, status: "EXPIRED", mode: "SINGLE", expiry: past},
		{id: 114, user: 7, status: "FAILED", mode: "SINGLE", expiry: past},
		{id: math.MaxUint64 - 1, user: 8, version: math.MaxUint64, status: "VERIFYING", mode: "MULTIPART", expiry: now},
		{id: math.MaxUint64, user: 8, version: math.MaxUint64, status: "VERIFYING", mode: "SINGLE", expiry: now},
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
			partSize, partCount = 16<<20, 2
		}
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
			VALUES(?,?,'local_minio','fixture-bucket',?,?)`, fixture.id, fixture.user, "expiry-"+strconv.FormatUint(fixture.id, 10), fixture.size)
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			file_size,part_size,part_count,status,reserved_bytes,lease_owner,lease_until,lease_version,next_attempt_at,expires_at)
			VALUES(?,?,?,?,?,'expiry.bin',?,?,?,?,?,?,?,?,?,?,?)`, fixture.id, fixture.user, deviceID, fixture.id, parentID,
			fixture.mode, fixture.size, partSize, partCount, fixture.status, fixture.size, leaseOwner, fixture.leaseAt,
			fixture.version, fixture.retryAt, fixture.expiry)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	readFacts := func(taskID uint64) (*domain.UploadTask, *domain.StoredObject, uint64) {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, taskID)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, task.StagedObjectID)
		if err != nil {
			t.Fatal(err)
		}
		var reserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=7").Scan(&reserved); err != nil {
			t.Fatal(err)
		}
		return task, object, reserved
	}
	beforeSingle, beforeSingleObject, beforeReserved := readFacts(100)
	beforeMultipart, beforeMultipartObject, _ := readFacts(150)

	// 3. 检查精确边界、同到期时间排序、批次截断和范围。
	want := []domain.UploadExpirationCandidate{
		{TaskID: 105, UserID: 8, StagedObjectID: 105, Status: domain.UploadStatusCreating, ExpiresAt: earlier.Add(-time.Microsecond), UploadMode: "SINGLE"},
		{TaskID: 150, UserID: 7, StagedObjectID: 150, LeaseVersion: 5, Status: domain.UploadStatusCompleting, ExpiresAt: earlier, UploadMode: "MULTIPART"},
		{TaskID: 122, UserID: 7, StagedObjectID: 122, LeaseVersion: 2, Status: domain.UploadStatusExpiring, ExpiresAt: past, UploadMode: "SINGLE"},
		{TaskID: 151, UserID: 8, StagedObjectID: 151, LeaseVersion: 7, Status: domain.UploadStatusExpiring, ExpiresAt: past, UploadMode: "MULTIPART"},
		{TaskID: 100, UserID: 7, StagedObjectID: 100, LeaseVersion: 3, Status: domain.UploadStatusUploading, ExpiresAt: now, UploadMode: "SINGLE"},
		{TaskID: 157, UserID: 7, StagedObjectID: 157, Status: domain.UploadStatusCreating, ExpiresAt: now, UploadMode: "MULTIPART"},
		{TaskID: math.MaxUint64 - 1, UserID: 8, StagedObjectID: math.MaxUint64 - 1, LeaseVersion: math.MaxUint64, Status: domain.UploadStatusVerifying, ExpiresAt: now, UploadMode: "MULTIPART"},
		{TaskID: math.MaxUint64, UserID: 8, StagedObjectID: math.MaxUint64, LeaseVersion: math.MaxUint64, Status: domain.UploadStatusVerifying, ExpiresAt: now, UploadMode: "SINGLE"},
	}
	queries := []struct {
		name string
		list func(*Store, context.Context, int) ([]domain.UploadExpirationCandidate, error)
		want []domain.UploadExpirationCandidate
	}{
		{"all modes", (*Store).ListUploadExpirationCandidates, want},
		{"single only", (*Store).ListSingleUploadExpirationCandidates, []domain.UploadExpirationCandidate{want[0], want[2], want[4], want[7]}},
		{"multipart only", (*Store).ListMultipartUploadExpirationCandidates, []domain.UploadExpirationCandidate{want[1], want[3], want[5], want[6]}},
	}
	for _, query := range queries {
		t.Run(query.name, func(t *testing.T) {
			for _, limit := range []int{1, 3, 100, 100} {
				candidates, err := query.list(store, ctx, limit)
				if err != nil || !reflect.DeepEqual(candidates, query.want[:min(limit, len(query.want))]) {
					t.Fatal("到期扫描候选范围、顺序或时间不符", limit, candidates, err)
				}
			}
			for _, limit := range []int{0, 101} {
				if candidates, err := query.list(store, ctx, limit); candidates != nil || err == nil {
					t.Fatal("扫描接受了无效上限", limit, candidates, err)
				}
			}
			if err := store.WithLoginUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
				if candidates, err := query.list(txStore, txContext, 1); candidates != nil || err == nil {
					t.Error("用户事务内允许了跨用户扫描", candidates, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			canceled, stop := context.WithCancel(ctx)
			stop()
			if candidates, err := query.list(store, canceled, 1); candidates != nil || !errors.Is(err, context.Canceled) {
				t.Fatal("扫描没有保留上下文取消", candidates, err)
			}
		})
	}
	afterSingle, afterSingleObject, afterReserved := readFacts(100)
	afterMultipart, afterMultipartObject, _ := readFacts(150)
	if !reflect.DeepEqual(afterSingle, beforeSingle) || !reflect.DeepEqual(afterSingleObject, beforeSingleObject) ||
		!reflect.DeepEqual(afterMultipart, beforeMultipart) || !reflect.DeepEqual(afterMultipartObject, beforeMultipartObject) || afterReserved != beforeReserved {
		t.Fatal("到期扫描修改了任务、对象或预占")
	}
	execute("SET timestamp = ?", now.Add(-time.Hour).Unix())
	for _, query := range queries {
		if candidates, err := query.list(store, ctx, 100); candidates == nil || len(candidates) != 0 || err != nil {
			t.Fatal("无到期候选时应返回空结果", query.name, candidates, err)
		}
	}
	t.Log("Multipart 限量前过滤模式，候选为 150、151、157、18446744073709551614；扫描未改变两种模式的任务、对象或账号预占")
}
