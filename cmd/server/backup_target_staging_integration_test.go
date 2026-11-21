package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerBackupTargetStagingIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后对齐隔离目标库的暂存对象")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 恢复独占目标环境，完成已发布对象的定位，再从源任务找到暂存对象。
	fixture, beforeTask := prepareBackupTargetPendingUpload(t, ctx)
	pool, target := fixture.database.pool, fixture.targetBucket
	userID, taskID := beforeTask.UserID, beforeTask.ID
	before := snapshotBackupTargetObjects(t, ctx, pool, userID)
	staged := before[beforeTask.StagedObjectID]
	if staged.ID == 0 || staged.Bucket != fixture.source.bucket.name || staged.Status != domain.StoredObjectStatusStaging ||
		staged.StorageVersionID != nil || staged.StorageETag != nil {
		t.Fatal("目标库没有保留未发送任务及其原暂存定位")
	}

	// 2. 用已发布键和错误旧桶测试拒绝路径，验证均未更新业务记录。
	occupied := staged
	occupied.ObjectKey = fixture.mapping.Objects[0].ObjectKey
	if err := alignBackupTargetStagingBucket(ctx, pool, target, beforeTask, occupied); err == nil || !strings.Contains(err.Error(), "已有对象版本") {
		t.Fatalf("目标键存在时没有拒绝对齐：%v", err)
	}
	stale := staged
	stale.Bucket = "stale-source-bucket"
	if err := alignBackupTargetStagingBucket(ctx, pool, target, beforeTask, stale); err == nil || !strings.Contains(err.Error(), "原定位或任务状态未匹配") {
		t.Fatalf("源桶失配时没有拒绝对齐：%v", err)
	}
	if !reflect.DeepEqual(snapshotBackupTargetObjects(t, ctx, pool, userID), before) ||
		!reflect.DeepEqual(readBackupUploadTask(t, ctx, pool, userID, taskID), beforeTask) {
		t.Fatal("拒绝路径修改了目标库对象或任务")
	}
	t.Log("目标已有版本和旧桶失配均被拒绝，目标对象和任务记录保持原值")

	// 3. 对齐实际暂存对象并读回，确认只改变其桶名，正文和预占尚未产生变化。
	if err := alignBackupTargetStagingBucket(ctx, pool, target, beforeTask, staged); err != nil {
		t.Fatal(err)
	}
	expected := staged
	expected.Bucket = target.name
	before[staged.ID] = expected
	if !reflect.DeepEqual(snapshotBackupTargetObjects(t, ctx, pool, userID), before) ||
		!reflect.DeepEqual(readBackupUploadTask(t, ctx, pool, userID, taskID), beforeTask) {
		t.Fatal("对齐后不止暂存对象桶名发生变化")
	}
	verifyBackupTargetVersions(t, ctx, target, fixture.mapping.Objects)
	inspectBackupDatabase(t, ctx, pool, userID)
	t.Logf("任务 %d 的暂存对象 %d 已由 %s 对齐到 %s，目标无正文版本，空版本和 ETag 保留，配额已用 10 B、预占 3 B",
		taskID, staged.ID, staged.Bucket, target.name)
}

func prepareBackupTargetPendingUpload(t *testing.T, ctx context.Context) (backupTargetRemapFixture, *domain.UploadTask) {
	t.Helper()
	fixture := prepareBackupTargetRemap(t, ctx)
	pool := fixture.database.pool
	updated, err := applyBackupTargetObjectMap(ctx, pool, fixture.targetBucket.name, fixture.manifest, fixture.mapping)
	if err != nil || updated != 3 {
		t.Fatalf("已发布对象定位基线不符，更新 %d 行，错误 %v", updated, err)
	}
	userID := backupFixtureID(t, fixture.source.login.User.ID)
	var taskID uint64
	if err := fixture.source.pool.QueryRowContext(ctx, "SELECT id FROM upload_tasks WHERE user_id=? AND status='UPLOADING'", userID).
		Scan(&taskID); err != nil {
		t.Fatal("查找源端未发送任务失败", err)
	}
	sourceTask := readBackupUploadTask(t, ctx, fixture.source.pool, userID, taskID)
	targetTask := readBackupUploadTask(t, ctx, pool, userID, taskID)
	if !reflect.DeepEqual(sourceTask, targetTask) || targetTask.Status != domain.UploadStatusUploading || targetTask.ReservedBytes != 3 {
		t.Fatal("目标库没有保留未发送任务及其预占")
	}
	return fixture, targetTask
}

func readBackupUploadTask(t *testing.T, ctx context.Context, pool *sql.DB, userID, taskID uint64) *domain.UploadTask {
	t.Helper()
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.FindUserUploadTask(ctx, userID, taskID)
	if err != nil || task == nil {
		t.Fatal("读取隔离上传任务失败", err)
	}
	return task
}

func alignBackupTargetStagingBucket(ctx context.Context, pool *sql.DB, target *serverUploadContentBucket, task *domain.UploadTask, staged domain.StoredObject) error {
	// 1. 限定当前任务的暂存对象，核对目标桶中该键的所有版本和删除标记。
	if staged.ID != task.StagedObjectID || staged.OwnerUserID != task.UserID || staged.Provider != "local_minio" ||
		staged.Bucket == "" || staged.Bucket == target.name || staged.ObjectKey == "" || task.ReservedBytes == 0 {
		return errors.New("目标桶或暂存对象定位无效")
	}
	for info := range target.client.ListObjects(ctx, target.name, minio.ListObjectsOptions{Prefix: staged.ObjectKey, Recursive: true, WithVersions: true}) {
		if info.Err != nil {
			return fmt.Errorf("核对目标桶暂存键失败: %w", info.Err)
		}
		if info.Key == staged.ObjectKey {
			return errors.New("目标暂存键已有对象版本或删除标记")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. 原子复核原定位、空内容元数据、任务状态及配额事实，只更新桶名。
	result, err := pool.ExecContext(ctx, `UPDATE stored_objects o
		JOIN upload_tasks t ON t.staged_object_id=o.id AND t.user_id=o.owner_user_id
		SET o.bucket=?
		WHERE o.id=? AND o.owner_user_id=? AND o.provider='local_minio' AND o.bucket=? AND o.object_key=?
		AND o.status='STAGING' AND o.storage_version_id IS NULL AND o.storage_etag IS NULL
		AND o.verified_sha256 IS NULL AND o.verified_at IS NULL AND o.unreferenced_at IS NULL AND o.gc_after IS NULL
		AND t.id=? AND t.status='UPLOADING' AND t.upload_mode='SINGLE' AND t.lease_version=0
		AND t.lease_owner IS NULL AND t.lease_until IS NULL AND t.result_entry_id IS NULL AND t.published_object_id IS NULL
		AND t.completed_at IS NULL AND t.expires_at>UTC_TIMESTAMP(6)
		AND t.file_size=o.file_size AND t.reserved_bytes=o.file_size AND t.reserved_bytes=?
		AND NOT EXISTS (SELECT 1 FROM file_entries f WHERE f.user_id=o.owner_user_id AND f.object_id=o.id)`,
		target.name, staged.ID, staged.OwnerUserID, staged.Bucket, staged.ObjectKey, task.ID, task.ReservedBytes)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("暂存对象的原定位或任务状态未匹配，更新 %d 行", affected)
	}
	return nil
}
