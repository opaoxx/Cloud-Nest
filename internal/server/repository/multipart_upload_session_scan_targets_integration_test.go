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

func TestListMultipartUploadSessionScanTargetsIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	const provider, bucket = "local_minio", "scan-bucket"
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	key := func(userID uint64, suffix int) string {
		return fmt.Sprintf("users/%d/objects/4b351d22-0cab-4a8d-89cc-%012x", userID, suffix)
	}
	const upperObjectID = uint64(math.MaxUint64)
	const upperTaskID = uint64(math.MaxUint64 - 1)

	// 1. 混合存储范围、账号、任务类型和对象生命周期。
	execute("UPDATE users SET status='DISABLED' WHERE id=8")
	for _, fixture := range []struct {
		objectID, taskID, userID       uint64
		provider, bucket, key          string
		mode, taskStatus, objectStatus string
		session                        *string
	}{
		{90, 190, 7, provider, bucket, key(7, 1), "SINGLE", "UPLOADING", "STAGING", nil},
		{91, 191, 7, "alternate", bucket, key(7, 2), "MULTIPART", "CREATING", "STAGING", nil},
		{92, 192, 7, provider, "other-bucket", key(7, 3), "MULTIPART", "CREATING", "STAGING", nil},
		{93, 0, 7, provider, bucket, key(7, 4), "", "", "STAGING", nil},
		{94, 194, 7, provider + " ", bucket, key(7, 5), "MULTIPART", "CREATING", "STAGING", nil},
		{95, 195, 7, provider, bucket + " ", key(7, 6), "MULTIPART", "CREATING", "STAGING", nil},
		{101, 201, 7, provider, bucket, "users/7/objects/legacy-key", "MULTIPART", "CREATING", "STAGING", nil},
		{102, 202, 7, provider, bucket, key(8, 7), "MULTIPART", "CREATING", "STAGING", nil},
		{103, 203, 7, provider, bucket, key(7, 8), "MULTIPART", "CREATING", "STAGING", nil},
		{104, 204, 8, provider, bucket, key(8, 9), "MULTIPART", "UPLOADING", "STAGING", new("fixture-session")},
		{105, 205, 7, provider, bucket, key(7, 10), "MULTIPART", "EXPIRED", "DELETED", new("expired-session")},
		{upperObjectID, upperTaskID, 7, provider, bucket, key(7, 11), "MULTIPART", "COMPLETED", "ORPHANED", new("completed-session")},
	} {
		deviceID, parentID := uint64(11), uint64(13)
		if fixture.userID == 8 {
			deviceID, parentID = 12, 14
		}
		fileSize := uint64(1)
		var partSize, partCount any
		if fixture.mode == "MULTIPART" {
			fileSize, partSize, partCount = domain.MaxSingleUploadSizeBytes+1, domain.MaxSingleUploadSizeBytes, 2
		}
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status) VALUES(?,?,?,?,?,?,?)`,
			fixture.objectID, fixture.userID, fixture.provider, fixture.bucket, fixture.key, fileSize, fixture.objectStatus)
		if fixture.taskID == 0 {
			continue
		}
		reserved := fileSize
		var publishedID, resultID any
		var completedAt *time.Time
		if fixture.taskStatus == "EXPIRED" || fixture.taskStatus == "COMPLETED" {
			reserved = 0
			completedAt = new(time.Now().UTC().Truncate(time.Microsecond))
		}
		if fixture.taskStatus == "COMPLETED" {
			publishedID, resultID = 29, 31
		}
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			storage_upload_id,expected_sha256,file_size,part_size,part_count,status,reserved_bytes,published_object_id,result_entry_id,completed_at,expires_at)
			VALUES(?,?,?,?,?,'fixture.bin',?,?,UNHEX(REPEAT('ab',32)),?,?,?,?,?,?,?,?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`,
			fixture.taskID, fixture.userID, deviceID, fixture.objectID, parentID, fixture.mode, fixture.session,
			fileSize, partSize, partCount, fixture.taskStatus, reserved, publishedID, resultID, completedAt)
	}

	readTask := func(userID, taskID uint64) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, userID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	readObject := func() *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, 7, 105)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	beforeTask, beforeObject := readTask(8, 204), readObject()
	var beforeReserved uint64
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=8").Scan(&beforeReserved); err != nil {
		t.Fatal(err)
	}
	valid := []domain.MultipartUploadSessionScanTarget{
		{ObjectID: 103, UserID: 7, TaskID: 203, ObjectKey: key(7, 8)},
		{ObjectID: 104, UserID: 8, TaskID: 204, ObjectKey: key(8, 9)},
		{ObjectID: 105, UserID: 7, TaskID: 205, ObjectKey: key(7, 10)},
		{ObjectID: upperObjectID, UserID: 7, TaskID: upperTaskID, ObjectKey: key(7, 11)},
	}
	// 2. 两条不合规记录占据第一页，游标仍能推进；末页支持完整无符号 ID。
	for _, fixture := range []struct {
		after, next uint64
		limit       int
		want        []domain.MultipartUploadSessionScanTarget
	}{
		{0, 102, 2, []domain.MultipartUploadSessionScanTarget{}},
		{102, 104, 2, valid[:2]},
		{104, 0, 2, valid[2:]},
		{upperObjectID, 0, 2, []domain.MultipartUploadSessionScanTarget{}},
		{0, 0, 100, valid},
	} {
		page, err := store.ListMultipartUploadSessionScanTargets(ctx, provider, bucket, fixture.after, fixture.limit)
		if err != nil || page == nil || page.NextAfterObjectID != fixture.next || !reflect.DeepEqual(page.Targets, fixture.want) {
			t.Fatal("分页结果或推进位置不符", fixture.after, fixture.limit, page, err)
		}
	}
	var afterReserved uint64
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage FROM users WHERE id=8").Scan(&afterReserved); err != nil ||
		afterReserved != beforeReserved || !reflect.DeepEqual(readTask(8, 204), beforeTask) || !reflect.DeepEqual(readObject(), beforeObject) {
		t.Fatal("只读分页改变了任务、对象或账号预占", err)
	}
	// 3. 拒绝无效范围及事务内跨用户扫描，保留取消原因。
	for _, limit := range []int{0, 101} {
		if page, err := store.ListMultipartUploadSessionScanTargets(ctx, provider, bucket, 0, limit); page != nil || err == nil {
			t.Fatal("接受了无效页大小", limit)
		}
	}
	if page, err := store.ListMultipartUploadSessionScanTargets(ctx, provider, "invalid\nbucket", 0, 1); page != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
		t.Fatal("接受了无效存储范围", page, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if page, err := store.ListMultipartUploadSessionScanTargets(canceled, provider, bucket, 0, 1); page != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("取消原因未保留", page, err)
	}
	if err := store.WithLoginUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
		if page, err := txStore.ListMultipartUploadSessionScanTargets(txContext, provider, bucket, 0, 1); page != nil || err == nil {
			t.Error("事务内仍可执行跨用户对象分页", page, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("首个空页推进至 102，继续读到 103、104、105 和最大无符号对象 ID，任务、对象和预占一致")
}
