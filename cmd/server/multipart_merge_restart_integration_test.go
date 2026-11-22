package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerMultipartMergeRestartIntegration(t *testing.T) {
	verifyDownload := os.Getenv("CLOUDNEST_MULTIPART_MERGE_RESTART_DOWNLOAD_TEST") == "1"
	verifyPublication := verifyDownload || os.Getenv("CLOUDNEST_MULTIPART_MERGE_RESTART_PUBLICATION_TEST") == "1"
	if os.Getenv("CLOUDNEST_MULTIPART_MERGE_RESTART_TEST") != "1" && !verifyPublication {
		t.Skip("按需启用 CLOUDNEST_MULTIPART_MERGE_RESTART_TEST、CLOUDNEST_MULTIPART_MERGE_RESTART_PUBLICATION_TEST 或 CLOUDNEST_MULTIPART_MERGE_RESTART_DOWNLOAD_TEST")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var firstPID int
	fixture := prepareBackupSourceWithServer(t, ctx, startIsolatedWebMinIO, func(t *testing.T, ctx context.Context, databaseName string) (backupSourceClient, func() (string, error)) {
		t.Helper()
		client, stop, pid := startIsolatedMultipartBackend(t, ctx, databaseName)
		firstPID = pid
		return client, stop
	})
	t.Logf("合并重启专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	api := fixture.api
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal("打开专用库仓储失败", err)
	}
	userID := backupFixtureID(t, fixture.login.User.ID)

	// 1. 通过正式接口创建并上传两片，仅对该对象的合并写回注入 MySQL 1205。
	const partSize uint64 = 16 << 20
	const fileSize = partSize + 1
	payload := make([]byte, fileSize)
	payload[0], payload[partSize] = 0x31, 0x72
	fileDigest := sha256.Sum256(payload)
	const uploadsPath = "/api/v1/uploads"
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	declaration := fmt.Sprintf(`{"parent_id":%q,"name":"merge-restart.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, fileSize, fileDigest)
	api.request(t, ctx, http.MethodPost, uploadsPath, "merge-restart", strings.NewReader(declaration), http.StatusCreated, &created)
	if created.RequestID == "" || created.Data.UploadMode != "MULTIPART" || created.Data.Task.Status != "CREATING" ||
		created.Data.PartSizeBytes != partSize || created.Data.PartCount != 2 {
		t.Fatal("合并重启测试未取得预期两片计划", created.Data)
	}
	taskID := backupFixtureID(t, created.Data.Task.ID)
	taskPath := uploadsPath + "/" + created.Data.Task.ID
	ready := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture, ready)
	pending, err := store.FindUserUploadTask(ctx, userID, taskID)
	if err != nil || pending == nil || pending.StagedObjectID == 0 || pending.ReservedBytes != fileSize {
		t.Fatal("未找到本轮原任务与暂存对象", err)
	}
	for index, contents := range [][]byte{payload[:partSize], payload[partSize:]} {
		signed, checksum := target.signPart(t, ctx, api, uint32(index+1), contents)
		putMultipartProbePart(t, ctx, api.httpClient, signed, checksum, contents, http.StatusOK, "")
	}
	var receipts dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
	if receipts.RequestID == "" || len(receipts.Data.Parts) != 2 {
		t.Fatal("未取得两条正式上传回执", receipts.Data)
	}
	if _, err := fixture.pool.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER fixture_merge_restart_write_fault
		BEFORE UPDATE ON stored_objects FOR EACH ROW
		BEGIN
			IF OLD.id = %d AND OLD.status = 'STAGING' AND NEW.status = 'VERIFYING' THEN
				SIGNAL SQLSTATE 'HY000' SET MYSQL_ERRNO=1205, MESSAGE_TEXT='fixture merge restart write fault';
			END IF;
		END`, pending.StagedObjectID)); err != nil {
		t.Fatal("无法在专用库安装定向写回故障", err)
	}
	accepted := submitMultipartCompletion(t, ctx, api, taskPath, ready, receipts.Data.Parts...)
	faulted := waitForMultipartMergeRestartTask(t, ctx, store, userID, taskID, func(task *domain.UploadTask) bool {
		return task.Status == domain.UploadStatusCompleting && task.NextAttemptAt != nil && task.LeaseUntil == nil
	})
	staged, err := store.FindUserStoredObject(ctx, userID, pending.StagedObjectID)
	if err != nil || staged == nil || staged.Status != domain.StoredObjectStatusStaging || staged.StorageVersionID != nil ||
		staged.StorageETag != nil || faulted.ReservedBytes != fileSize || faulted.LeaseOwner != nil ||
		faulted.LastErrorCode == nil || *faulted.LastErrorCode != "MERGE_RECHECK_REQUIRED" {
		t.Fatal("首次写回故障后的任务、对象或重试状态不符", faulted, err)
	}
	merged, err := fixture.bucket.client.StatObject(ctx, target.bucket, target.objectKey, minio.StatObjectOptions{})
	if err != nil || merged.VersionID == "" || merged.VersionID == "null" || merged.ETag == "" || merged.Size != int64(fileSize) {
		t.Fatal("写回失败前未确认 MinIO 已完成实际合并", err)
	}
	var dbTime time.Time
	if err := fixture.pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&dbTime); err != nil || !faulted.NextAttemptAt.After(dbTime) {
		t.Fatal("首个进程尚未留下有效的重试等待窗口", err)
	}

	// 2. 等待首个独立进程退出，再解除夹具故障并模拟本任务的重试时间到达。
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("首次后端进程退出失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	if !strings.Contains(logs, `"retries_scheduled":1`) {
		t.Fatal("首次进程未报告已安排一条合并重试")
	}
	t.Logf("首个后端 PID %d 已退出，任务 %d 仍为 COMPLETING，已合并版本 %s，ETag %s，租约版本 %d",
		firstPID, taskID, merged.VersionID, merged.ETag, faulted.LeaseVersion)
	if _, err := fixture.pool.ExecContext(ctx, "DROP TRIGGER fixture_merge_restart_write_fault"); err != nil {
		t.Fatal("解除本轮写回故障失败", err)
	}
	update, err := fixture.pool.ExecContext(ctx, `UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)
		WHERE id=? AND user_id=? AND storage_upload_id=? AND status='COMPLETING'
		  AND lease_version=? AND lease_owner IS NULL AND lease_until IS NULL AND next_attempt_at=?`,
		taskID, userID, target.uploadID, faulted.LeaseVersion, *faulted.NextAttemptAt)
	if err != nil {
		t.Fatal("设置原任务的隔离重试时间失败", err)
	}
	if updated, err := update.RowsAffected(); err != nil || updated != 1 {
		t.Fatal("隔离环境到期模拟未精确命中原任务", updated, err)
	}
	restartedAddress, stopRestarted, restartedPID, releaseVerification := launchIsolatedMultipartBackend(t, ctx, fixture.databaseName, "hold-verification")
	if restartedPID == firstPID {
		t.Fatal("恢复未启动新的后端进程")
	}

	// 3. 新进程先恢复合并结果，固定在校验 worker 启动前独立检查数据库和存储。
	recovered := waitForMultipartMergeRestartTask(t, ctx, store, userID, taskID, func(task *domain.UploadTask) bool {
		return task.Status == domain.UploadStatusVerifying
	})
	object, err := store.FindUserStoredObject(ctx, userID, pending.StagedObjectID)
	if err != nil || object == nil || object.Status != domain.StoredObjectStatusVerifying ||
		object.ObjectKey != target.objectKey || object.Bucket != target.bucket || object.VerifiedAt != nil ||
		object.StorageVersionID == nil || *object.StorageVersionID != merged.VersionID ||
		object.StorageETag == nil || *object.StorageETag != merged.ETag ||
		recovered.StagedObjectID != pending.StagedObjectID || recovered.LeaseVersion != faulted.LeaseVersion+1 ||
		recovered.StorageUploadID == nil || *recovered.StorageUploadID != target.uploadID ||
		recovered.NextAttemptAt != nil || recovered.LeaseOwner != nil || recovered.LeaseUntil != nil ||
		recovered.ResultEntryID != nil || recovered.ReservedBytes != fileSize {
		t.Fatal("新进程没有恢复原版本与待校验状态", recovered, err)
	}
	verifyMultipartCompletionSnapshot(t, ctx, fixture, receipts.Data.Parts, ready, accepted)
	versionCount := 0
	for version := range fixture.bucket.client.ListObjects(ctx, target.bucket, minio.ListObjectsOptions{
		Prefix: target.objectKey, Recursive: true, WithVersions: true,
	}) {
		if version.Err != nil || version.Key != target.objectKey || version.VersionID != merged.VersionID || version.IsDeleteMarker {
			t.Fatal("恢复后发现意外存储版本", version.Err)
		}
		versionCount++
	}
	var used, reserved, entries uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE') FROM users WHERE id=?`, userID).
		Scan(&used, &reserved, &entries); err != nil || used != 0 || reserved != fileSize || entries != 0 || versionCount != 1 {
		t.Fatal("恢复后的配额、条目或存储版本数量不符", err, used, reserved, entries, versionCount)
	}
	t.Logf("新后端 PID %d 将任务 %d 恢复至 VERIFYING，原版本 %s、ETag %s，租约版本 %d，存储版本 %d、发布条目 %d、已用 %d B、预占 %d B",
		restartedPID, taskID, merged.VersionID, merged.ETag, recovered.LeaseVersion, versionCount, entries, used, reserved)

	if verifyPublication {
		// 4. 放行同一进程的校验及发布，核对文件引用原版本且重试只返回同一结果。
		if err := releaseVerification(); err != nil {
			t.Fatal("放行恢复进程校验失败", err)
		}
		restarted := waitForBackupFixtureClient(t, ctx, restartedAddress)
		login := loginBackupFixture(t, ctx, &restarted, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
		if login.User.ID != fixture.login.User.ID || login.RootEntryID != fixture.login.RootEntryID {
			t.Fatal("恢复进程登录回执未绑定原账号及根目录")
		}
		completed := awaitResumedMultipartPublication(t, ctx, restarted, taskPath, accepted)
		publication := verifyResumedMultipartPublication(t, ctx, fixture, restarted, completed, fileDigest)
		if publication.objectID != pending.StagedObjectID || publication.objectVersion != merged.VersionID ||
			publication.objectPath != "/"+target.bucket+"/"+target.objectKey {
			t.Fatal("发布条目未引用故障前合并的原对象版本")
		}
		verifyResumedMultipartCompletionReplay(t, ctx, fixture, restarted, taskPath, completed)
		t.Logf("重启发布保留原合并版本 %s，原任务 %s 只发布条目 %s", publication.objectVersion, completed.ID, publication.entry.ID)

		if verifyDownload {
			// 5. 从正式授权读取已核对的原版本，独立比对下载正文与合成源。
			verifyResumedMultipartDownload(t, ctx, restarted, publication, fileDigest)
		}
	}

	restartLogs, err := stopRestarted()
	if err != nil || !strings.Contains(restartLogs, `"verifying":1`) {
		t.Fatal("重启进程未正常退出或未汇报恢复结果", err)
	}
	if verifyPublication {
		verifyBackupSourceStopped(t, "http://"+restartedAddress, restartLogs)
	}
	t.Logf("新后端 PID %d 已退出", restartedPID)
}

func waitForMultipartMergeRestartTask(t *testing.T, ctx context.Context, store *repository.Store, userID, taskID uint64,
	ready func(*domain.UploadTask) bool) *domain.UploadTask {
	t.Helper()
	wait, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	interval := time.NewTicker(100 * time.Millisecond)
	defer interval.Stop()
	for {
		task, err := store.FindUserUploadTask(wait, userID, taskID)
		if err != nil || task == nil {
			t.Fatal("读取原合并任务失败", err)
		}
		if ready(task) {
			return task
		}
		if task.Status != domain.UploadStatusCompleting {
			t.Fatal("等待合并重启时遇到非预期阶段", task.Status)
		}
		select {
		case <-wait.Done():
			t.Fatal("等待合并重启观察点超时", task.Status, task.LastErrorCode)
		case <-interval.C:
		}
	}
}
