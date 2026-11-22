package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartRestartReconciliationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_RESTART_RECONCILIATION_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_RESTART_RECONCILIATION_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 创建合成任务，等待后台绑定 MinIO 分片会话。
	fixture := prepareBackupSource(t, ctx)
	t.Logf("分片重启专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	const partSize = 16 << 20
	payload := make([]byte, partSize+1)
	payload[0], payload[partSize] = 0x31, 0x32
	fileSize := uint64(len(payload))
	const filename = "multipart-restart-fixture.bin"
	declaration := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, filename, fileSize, sha256.Sum256(payload))
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	fixture.api.request(t, ctx, http.MethodPost, "/api/v1/uploads", "multipart-restart-fixture",
		strings.NewReader(declaration), http.StatusCreated, &created)
	taskID := backupFixtureID(t, created.Data.Task.ID)
	if created.RequestID == "" || created.Data.Task.Status != "CREATING" ||
		created.Data.UploadMode != "MULTIPART" || created.Data.PartSizeBytes != partSize || created.Data.PartCount != 2 {
		t.Fatal("创建响应未交付固定的两片计划", created.Data)
	}
	taskPath := "/api/v1/uploads/" + created.Data.Task.ID
	readyTask := waitForCreateUploadInitialization(t, ctx, fixture.api, taskPath)
	if readyTask.ID != created.Data.Task.ID || readyTask.ParentID != fixture.login.RootEntryID || readyTask.UploadMode != "MULTIPART" ||
		readyTask.SizeBytes != fileSize || readyTask.RequestedName != filename || readyTask.ResultEntryID != nil ||
		readyTask.CompletedAt != nil || !readyTask.ExpiresAt.Equal(created.Data.ExpiresAt) {
		t.Fatal("就绪任务与原声明不符", readyTask)
	}
	var storageUploadID, objectKey string
	if err := fixture.pool.QueryRowContext(ctx, `SELECT task.storage_upload_id, obj.object_key
		FROM upload_tasks task JOIN stored_objects obj ON obj.id=task.staged_object_id
		WHERE task.id=? AND task.user_id=? AND obj.bucket=?`, taskID, fixture.login.User.ID, fixture.bucket.name).
		Scan(&storageUploadID, &objectKey); err != nil || storageUploadID == "" || objectKey == "" {
		t.Fatal("专用桶分片会话尚未绑定", err)
	}
	core := minio.Core{Client: fixture.bucket.client}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanup, fixture.bucket.name, objectKey, storageUploadID); err != nil {
			t.Error("合成分片会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})

	// 2. 签发并上传第一片，记录重启前的任务与回执。
	firstPart := payload[:partSize]
	checksum := multipartProbeSHA256(firstPart)
	var signed dto.UploadPartURLsResponse
	fixture.api.request(t, ctx, http.MethodPost, taskPath+"/part-urls", "",
		strings.NewReader(fmt.Sprintf(`{"parts":[{"part_number":1,"sha256":"%x"}]}`, sha256.Sum256(firstPart))), http.StatusOK, &signed)
	if signed.RequestID == "" || len(signed.Data.Parts) != 1 || !signed.Data.ExpiresAt.Equal(readyTask.ExpiresAt) {
		t.Fatal("第一片签发范围或任务期限不符")
	}
	part := signed.Data.Parts[0]
	uploadURL, err := url.Parse(part.URL)
	if err != nil || uploadURL.Scheme+"://"+uploadURL.Host != os.Getenv("CLOUDNEST_S3_PUBLIC_ENDPOINT") || uploadURL.User != nil ||
		uploadURL.Path != "/"+fixture.bucket.name+"/"+objectKey || uploadURL.Query().Get("uploadId") != storageUploadID ||
		uploadURL.Query().Get("partNumber") != "1" || part.PartNumber != 1 || part.SizeBytes != partSize ||
		part.RequiredHeaders.ChecksumSHA256 != checksum {
		t.Fatal("签名没有限定在本轮隔离会话的第一片")
	}
	etag := putMultipartProbePart(t, ctx, fixture.api.httpClient, uploadURL, checksum, firstPart, http.StatusOK, "")
	wantReceipt := dto.UploadPartReceiptData{PartNumber: 1, SizeBytes: partSize, ETag: etag, ChecksumSHA256: checksum}
	readReceipts := func(api backupSourceClient) dto.UploadPartsData {
		t.Helper()
		var receipts dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
		if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 ||
			!slices.Equal(receipts.Data.Parts, []dto.UploadPartReceiptData{wantReceipt}) {
			t.Fatal("分片计划或实际回执不符", receipts.Data)
		}
		return receipts.Data
	}
	beforeRestart := readReceipts(fixture.api)
	var beforeProfile dto.UserProfileResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &beforeProfile)
	if beforeProfile.Data.UsedBytes != 0 || beforeProfile.Data.ReservedBytes != fileSize {
		t.Fatal("重启前的已用与预占额度不符")
	}

	// 3. 保留隔离库和桶，重启服务并重新登录。
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("第一次服务停机失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	restarted, stopRestarted := startBackupFixtureServer(t, ctx, fixture.databaseName)
	login := loginBackupFixture(t, ctx, &restarted, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if !reflect.DeepEqual(login.UserProfileData, beforeProfile.Data) {
		t.Fatal("重启后账号、根目录或预占额度发生变化")
	}

	// 4. 用原任务 ID 核对回执、配额和文件发布状态。
	var afterRestart dto.UploadTaskResponse
	restarted.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &afterRestart)
	if afterRestart.RequestID == "" || !reflect.DeepEqual(afterRestart.Data, readyTask) ||
		!reflect.DeepEqual(readReceipts(restarted), beforeRestart) {
		t.Fatal("重启后的任务或分片回执与基线不一致")
	}
	var boundSession string
	var completionRows, childEntries uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT storage_upload_id,
		(SELECT COUNT(*) FROM upload_parts WHERE task_id=task.id),
		(SELECT COUNT(*) FROM file_entries WHERE user_id=task.user_id AND parent_id=task.target_parent_id)
		FROM upload_tasks task WHERE id=? AND user_id=?`, taskID, login.User.ID).
		Scan(&boundSession, &completionRows, &childEntries); err != nil ||
		boundSession != storageUploadID || completionRows != 0 || childEntries != 0 {
		t.Fatal("分片会话变化，或意外生成完成快照或文件条目", err, completionRows, childEntries)
	}
	logs, err = stopRestarted()
	if err != nil {
		t.Fatal("重启实例停机失败", err)
	}
	verifyBackupSourceStopped(t, restarted.baseURL, logs)
	t.Logf("重启前 %s，重启后 %s；任务 %d 保持 UPLOADING，仅第 1/2 片 %d B，ETag %s，SHA-256 %s，已用 %d B，预占 %d B，完成回执与发布条目均为 0",
		fixture.api.baseURL, restarted.baseURL, taskID, wantReceipt.SizeBytes, etag, checksum, login.UsedBytes, login.ReservedBytes)
}
