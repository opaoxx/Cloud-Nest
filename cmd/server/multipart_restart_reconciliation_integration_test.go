package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
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
	fileDigest := sha256.Sum256(payload)
	const filename = "multipart-restart-fixture.bin"
	sourcePath := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(sourcePath, payload, 0600); err != nil {
		t.Fatal("无法保存合成源文件", err)
	}
	declaration := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, filename, fileSize, fileDigest)
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
		if err := core.AbortMultipartUpload(cleanup, fixture.bucket.name, objectKey, storageUploadID); err != nil &&
			minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("合成分片会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})

	// 2. 签发并上传第一片，记录重启前的任务与回执。
	uploadPart := func(api backupSourceClient, number uint32, contents []byte) dto.UploadPartReceiptData {
		t.Helper()
		checksum := multipartProbeSHA256(contents)
		var signed dto.UploadPartURLsResponse
		api.request(t, ctx, http.MethodPost, taskPath+"/part-urls", "",
			strings.NewReader(fmt.Sprintf(`{"parts":[{"part_number":%d,"sha256":"%x"}]}`, number, sha256.Sum256(contents))),
			http.StatusOK, &signed)
		if signed.RequestID == "" || len(signed.Data.Parts) != 1 || !signed.Data.ExpiresAt.Equal(readyTask.ExpiresAt) {
			t.Fatal("分片签发范围或任务期限不符", number)
		}
		part := signed.Data.Parts[0]
		uploadURL, err := url.Parse(part.URL)
		if err != nil || uploadURL.Scheme+"://"+uploadURL.Host != os.Getenv("CLOUDNEST_S3_PUBLIC_ENDPOINT") || uploadURL.User != nil ||
			uploadURL.Path != "/"+fixture.bucket.name+"/"+objectKey || uploadURL.Query().Get("uploadId") != storageUploadID ||
			uploadURL.Query().Get("partNumber") != fmt.Sprint(number) || part.PartNumber != number ||
			part.SizeBytes != uint64(len(contents)) || part.RequiredHeaders.ChecksumSHA256 != checksum {
			t.Fatal("签名没有限定在本轮隔离会话的预期分片", number)
		}
		etag := putMultipartProbePart(t, ctx, api.httpClient, uploadURL, checksum, contents, http.StatusOK, "")
		return dto.UploadPartReceiptData{PartNumber: number, SizeBytes: part.SizeBytes, ETag: etag, ChecksumSHA256: checksum}
	}
	firstReceipt := uploadPart(fixture.api, 1, payload[:partSize])
	readReceipts := func(api backupSourceClient, expected ...dto.UploadPartReceiptData) dto.UploadPartsData {
		t.Helper()
		var receipts dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
		if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 ||
			!slices.Equal(receipts.Data.Parts, expected) {
			t.Fatal("分片计划或实际回执不符", receipts.Data)
		}
		return receipts.Data
	}
	beforeRestart := readReceipts(fixture.api, firstReceipt)
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

	// 4. 用原任务 ID 核对已上传首片，再重新读取源文件验证大小和整文件摘要。
	var afterRestart dto.UploadTaskResponse
	restarted.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &afterRestart)
	afterRestartParts := readReceipts(restarted, firstReceipt)
	if afterRestart.RequestID == "" || !reflect.DeepEqual(afterRestart.Data, readyTask) ||
		!reflect.DeepEqual(afterRestartParts, beforeRestart) {
		t.Fatal("重启后的任务或分片回执与基线不一致")
	}
	t.Logf("重启前 %s，重启后 %s；任务 %d 保持 UPLOADING，仅第 1/2 片 %d B，ETag %s，SHA-256 %s",
		fixture.api.baseURL, restarted.baseURL, taskID, firstReceipt.SizeBytes, firstReceipt.ETag, firstReceipt.ChecksumSHA256)
	resumedSource, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal("重读合成源文件失败", err)
	}
	if uint64(len(resumedSource)) != afterRestart.Data.SizeBytes || sha256.Sum256(resumedSource) != fileDigest {
		t.Fatal("合成源大小或整文件摘要已变化，不能续传")
	}

	// 5. 依据计划找出缺失编号，仅对该片签发并上传，最后比对全部回执。
	var missingParts []uint32
	for number := uint32(1); number <= afterRestartParts.PartCount; number++ {
		if !slices.ContainsFunc(afterRestartParts.Parts, func(receipt dto.UploadPartReceiptData) bool {
			return receipt.PartNumber == number
		}) {
			missingParts = append(missingParts, number)
		}
	}
	if !slices.Equal(missingParts, []uint32{2}) {
		t.Fatal("缺失分片应只有第二片", missingParts)
	}
	number := missingParts[0]
	tailSize, err := domain.ExpectedMultipartUploadPartSize(fileSize, afterRestartParts.PartSizeBytes, afterRestartParts.PartCount, number)
	if err != nil || tailSize != 1 {
		t.Fatal("缺失尾片字节数不符", tailSize, err)
	}
	offset := uint64(number-1) * afterRestartParts.PartSizeBytes
	tailReceipt := uploadPart(restarted, number, resumedSource[offset:offset+tailSize])
	resumedParts := readReceipts(restarted, firstReceipt, tailReceipt)
	var afterResume dto.UploadTaskResponse
	restarted.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &afterResume)
	var afterProfile dto.UserProfileResponse
	restarted.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &afterProfile)
	if !reflect.DeepEqual(afterResume.Data, readyTask) || !reflect.DeepEqual(afterProfile.Data, beforeProfile.Data) {
		t.Fatal("补传尾片后任务状态、期限或配额发生变化")
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
	t.Logf("原任务 %d 只补传第 %d 片 %d B，ETag %s，SHA-256 %s；两片齐备，首片回执保持原值，已用 %d B，预占 %d B，完成回执与发布条目均为 0",
		taskID, tailReceipt.PartNumber, tailReceipt.SizeBytes, tailReceipt.ETag, tailReceipt.ChecksumSHA256,
		afterProfile.Data.UsedBytes, afterProfile.Data.ReservedBytes)

	// 6. 使用补传后查询的编号和 ETag 提交原任务，确认 202 交付已提交的完成意图。
	firstPart, lastPart := resumedParts.Parts[0], resumedParts.Parts[1]
	completeBody := fmt.Sprintf(`{"parts":[{"part_number":%d,"etag":%q},{"part_number":%d,"etag":%q}]}`,
		firstPart.PartNumber, firstPart.ETag, lastPart.PartNumber, lastPart.ETag)
	var accepted dto.UploadTaskResponse
	restarted.request(t, ctx, http.MethodPost, taskPath+"/complete", "", strings.NewReader(completeBody), http.StatusAccepted, &accepted)
	if accepted.RequestID == "" || accepted.Data.ID != readyTask.ID || accepted.Data.UploadMode != "MULTIPART" ||
		accepted.Data.Status != "COMPLETING" || accepted.Data.SizeBytes != fileSize || !accepted.Data.ExpiresAt.Equal(readyTask.ExpiresAt) ||
		accepted.Data.ResultEntryID != nil || accepted.Data.CompletedAt != nil {
		t.Fatal("续传后的完成请求未交付原任务的 COMPLETING 快照", accepted.Data)
	}

	// 7. 关停后从独立数据库连接读取冻结的回执，避免依赖合并 worker 的时序。
	logs, err = stopRestarted()
	if err != nil {
		t.Fatal("重启实例停机失败", err)
	}
	verifyBackupSourceStopped(t, restarted.baseURL, logs)
	rows, err := fixture.pool.QueryContext(ctx, `SELECT part_number,size,etag,checksum_algorithm,checksum_value,observed_at
		FROM upload_parts WHERE task_id=? ORDER BY part_number`, taskID)
	if err != nil {
		t.Fatal("关停后读取本次完成快照失败", err)
	}
	defer rows.Close()
	var firstObservedAt time.Time
	for _, expected := range resumedParts.Parts {
		if !rows.Next() {
			t.Fatal("完成快照缺少预期分片", expected.PartNumber, rows.Err())
		}
		var persisted dto.UploadPartReceiptData
		var algorithm string
		var observedAt time.Time
		if err := rows.Scan(&persisted.PartNumber, &persisted.SizeBytes, &persisted.ETag,
			&algorithm, &persisted.ChecksumSHA256, &observedAt); err != nil {
			t.Fatal("读取完成回执字段失败", err)
		}
		if firstObservedAt.IsZero() {
			firstObservedAt = observedAt
		}
		if persisted != expected || algorithm != "SHA256" || !observedAt.Equal(firstObservedAt) ||
			observedAt.Before(afterResume.Data.UpdatedAt) || observedAt.After(accepted.Data.UpdatedAt) {
			t.Fatal("已保存回执与续传查询或本轮核对时间不一致", persisted, algorithm, observedAt)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatal("完成快照包含额外分片或读取失败", rows.Err())
	}
	t.Logf("原任务 %d 返回 202 COMPLETING，关停后两条完成快照仍匹配，分片编号 1、2，字节数 %d、%d，共用核对时间 %s",
		taskID, firstPart.SizeBytes, lastPart.SizeBytes, firstObservedAt.Format(time.RFC3339Nano))
}
