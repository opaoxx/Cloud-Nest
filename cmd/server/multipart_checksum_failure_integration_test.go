package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartChecksumFailureIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CHECKSUM_FAILURE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_CHECKSUM_FAILURE_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("整文件失配专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	api := fixture.api

	// 1. 用不同的整文件摘要创建任务，分片摘要仍与各自的正文一致。
	const partSize = domain.MaxSingleUploadSizeBytes
	const fileSize = partSize + 1
	payload := make([]byte, fileSize)
	payload[0], payload[partSize] = 0x31, 0x32
	actualDigest := sha256.Sum256(payload)
	declaredDigest := actualDigest
	declaredDigest[0] ^= 1
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":"multipart-checksum-failure-fixture.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, fileSize, declaredDigest)
	api.request(t, ctx, http.MethodPost, "/api/v1/uploads", "multipart-checksum-failure-fixture", strings.NewReader(body), http.StatusCreated, &created)
	if created.RequestID == "" || created.Data.UploadMode != "MULTIPART" || created.Data.Task.Status != "CREATING" ||
		created.Data.PartSizeBytes != partSize || created.Data.PartCount != 2 {
		t.Fatal("摘要失配夹具没有取得预期的两片计划", created.Data)
	}
	taskPath := "/api/v1/uploads/" + created.Data.Task.ID
	readyTask := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture, readyTask)
	putPart := func(number uint32, contents []byte) dto.UploadPartReceiptData {
		t.Helper()
		signed, checksum := target.signPart(t, ctx, api, number, contents)
		etag := putMultipartProbePart(t, ctx, api.httpClient, signed, checksum, contents, http.StatusOK, "")
		return dto.UploadPartReceiptData{PartNumber: number, SizeBytes: uint64(len(contents)), ETag: etag, ChecksumSHA256: checksum}
	}
	received := []dto.UploadPartReceiptData{putPart(1, payload[:partSize]), putPart(2, payload[partSize:])}
	var receipts dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
	if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 ||
		!slices.Equal(receipts.Data.Parts, received) {
		t.Fatal("两片均成功上传后，正式回执不符", receipts.Data)
	}
	checkQuota := func(expectedReserved uint64) {
		t.Helper()
		var profile dto.UserProfileResponse
		api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
		if profile.RequestID == "" || profile.Data.User.ID != fixture.login.User.ID ||
			profile.Data.UsedBytes != 0 || profile.Data.ReservedBytes != expectedReserved {
			t.Fatal("摘要失配任务的配额不符", profile.Data.UsedBytes, profile.Data.ReservedBytes)
		}
	}
	checkQuota(fileSize)

	// 2. 用正式查询的有效回执提交完成，等待后台实际校验整文件。
	accepted := submitMultipartCompletion(t, ctx, api, taskPath, readyTask, receipts.Data.Parts...)
	t.Logf("原任务 %s 两片摘要校验通过，完成接口返回 202 COMPLETING，声明 SHA-256 %x，实际 SHA-256 %x",
		accepted.ID, declaredDigest, actualDigest)
	failed := awaitMultipartChecksumFailure(t, ctx, api, taskPath)
	if failed.ID != accepted.ID || failed.UploadMode != "MULTIPART" || failed.SizeBytes != fileSize ||
		failed.ResultEntryID != nil || failed.ResolvedName != nil || failed.CompletedAt == nil ||
		failed.CompletedAt.Before(accepted.UpdatedAt) || failed.LastError == nil || failed.LastError.Code != domain.UploadChecksumMismatchCode {
		t.Fatal("完成受理后没有交付原任务的摘要失配终态", failed)
	}

	// 3. 核对预占已返还，任务没有发布条目，已合并对象已登记延迟回收。
	checkQuota(0)
	var reserved, publishedFiles, completedParts uint64
	var resultAbsent, leaseReleased, gcScheduled bool
	var objectState, objectVersion string
	if err := fixture.pool.QueryRowContext(ctx, `SELECT task.reserved_bytes,
		task.result_entry_id IS NULL AND task.published_object_id IS NULL,
		task.lease_owner IS NULL AND task.lease_until IS NULL, obj.status,obj.storage_version_id,obj.gc_after IS NOT NULL,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=task.user_id AND entry_type='FILE'),
		(SELECT COUNT(*) FROM upload_parts WHERE task_id=task.id)
		FROM upload_tasks task JOIN stored_objects obj ON obj.id=task.staged_object_id AND obj.owner_user_id=task.user_id
		WHERE task.id=? AND task.user_id=? AND obj.bucket=? AND obj.object_key=?`,
		failed.ID, fixture.login.User.ID, fixture.bucket.name, target.objectKey).
		Scan(&reserved, &resultAbsent, &leaseReleased, &objectState, &objectVersion, &gcScheduled, &publishedFiles, &completedParts); err != nil ||
		reserved != 0 || !resultAbsent || !leaseReleased || objectState != "ORPHANED" || objectVersion == "" ||
		!gcScheduled || publishedFiles != 0 || completedParts != 2 {
		t.Fatal("整文件摘要失配后任务、配额、文件或暂存对象不符", err, objectState, publishedFiles, completedParts)
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("原任务 %s 最终 FAILED CHECKSUM_MISMATCH，发布条目 %d，全额释放 %d B，已用和预占均为 0，已合并对象为 ORPHANED 并登记回收",
		failed.ID, publishedFiles, fileSize)
}

func awaitMultipartChecksumFailure(t *testing.T, ctx context.Context, api backupSourceClient, taskPath string) dto.UploadTaskData {
	t.Helper()
	waitContext, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	// 1. 通过任务查询等待失败终态，遇到其他终态立即报告。
	for {
		var response dto.UploadTaskResponse
		api.request(t, waitContext, http.MethodGet, taskPath, "", nil, http.StatusOK, &response)
		if response.RequestID == "" {
			t.Fatal("任务查询响应缺少请求 ID")
		}
		switch response.Data.Status {
		case "FAILED":
			return response.Data
		case "COMPLETING", "VERIFYING":
		default:
			t.Fatal("摘要失配任务进入意外状态", response.Data.Status, response.Data.LastError)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("后台未在期限内完成整文件摘要核对", response.Data.Status)
		case <-poll.C:
		}
	}
}
