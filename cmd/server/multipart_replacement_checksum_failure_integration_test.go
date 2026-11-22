package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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

func TestServerMultipartReplacementChecksumFailureIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_REPLACEMENT_CHECKSUM_FAILURE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_REPLACEMENT_CHECKSUM_FAILURE_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 发布原文件，记录版本、对象、历史和变更位置。
	fixture := prepareReplacementPublicationFixture(t, ctx)
	t.Logf("分片替换摘要失配专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	originalSize := uint64(len(fixture.original))
	fixture.checkQuota(t, ctx, originalSize, 0)
	readProtectedState := func() string {
		t.Helper()
		var snapshot string
		err := fixture.pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(
			entry.object_id,entry.revision,entry.name,entry.parent_id,entry.status,entry.file_size,entry.updated_at,
			account.used_storage,account.change_seq,
			(SELECT COUNT(*) FROM file_entries WHERE user_id=account.id AND entry_type='FILE'),
			(SELECT COUNT(*) FROM file_versions WHERE user_id=account.id),
			(SELECT COUNT(*) FROM change_events WHERE user_id=account.id))
			FROM file_entries entry JOIN users account ON account.id=entry.user_id WHERE entry.id=? AND account.id=?`,
			fixture.target.ID, fixture.userID).Scan(&snapshot)
		if err != nil {
			t.Fatal("读取原文件、历史和变更基线失败", err)
		}
		return snapshot
	}
	baseline := readProtectedState()

	// 2. 用错误的整文件摘要创建任务，用正确的分片摘要上传两片并提交完成。
	const partSize = domain.MaxSingleUploadSizeBytes
	const fileSize = partSize + 1
	payload := make([]byte, fileSize)
	copy(payload, "CloudNest multipart replacement checksum failure")
	payload[partSize] = '2'
	actualDigest := sha256.Sum256(payload)
	declaredDigest := actualDigest
	declaredDigest[0] ^= 1
	body := fmt.Sprintf(`{"expected_revision":"%d","size_bytes":%d,"sha256":"%x"}`, fixture.target.Revision, fileSize, declaredDigest)
	var response struct {
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	fixture.api.request(t, ctx, http.MethodPost, fixture.entryPath+"/content-replacement", "multipart-replacement-checksum-failure",
		strings.NewReader(body), http.StatusCreated, &response)
	created, err := dto.RestoreCreateMultipartUploadReplacementData(response.Data)
	if err != nil || response.RequestID == "" || created.PartSizeBytes != partSize || created.PartCount != 2 {
		t.Fatal("正式创建未返回两片替换计划", err)
	}
	taskID := backupFixtureID(t, created.Task.ID)
	if err := dto.ValidateCreateUploadReplacementSnapshot(response.Data, taskID, fixture.target.ID, fixture.target.Revision, fileSize); err != nil {
		t.Fatal("替换任务未绑定原文件和版本", err)
	}
	fixture.checkQuota(t, ctx, originalSize, fileSize)
	taskPath := "/api/v1/uploads/" + created.Task.ID
	ready := waitForCreateUploadInitialization(t, ctx, fixture.api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture.backupSourceFixture, ready)
	var received []dto.UploadPartReceiptData
	for index, contents := range [][]byte{payload[:partSize], payload[partSize:]} {
		number := uint32(index + 1)
		signed, checksum := target.signPart(t, ctx, fixture.api, number, contents)
		etag := putMultipartProbePart(t, ctx, fixture.api.httpClient, signed, checksum, contents, http.StatusOK, "")
		received = append(received, dto.UploadPartReceiptData{
			PartNumber: number, SizeBytes: uint64(len(contents)), ETag: etag, ChecksumSHA256: checksum,
		})
	}
	var receipts dto.UploadPartsResponse
	fixture.api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
	if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 || !slices.Equal(receipts.Data.Parts, received) {
		t.Fatal("服务端分片回执与编号、长度、摘要或 ETag 不符", receipts.Data)
	}
	accepted := submitMultipartCompletion(t, ctx, fixture.api, taskPath, ready, receipts.Data.Parts...)
	t.Logf("任务 %s 两片各 %d B 和 %d B，回执匹配，完成返回 202 COMPLETING；声明 SHA-256 %x，实际 SHA-256 %x",
		accepted.ID, received[0].SizeBytes, received[1].SizeBytes, declaredDigest, actualDigest)

	// 3. 等待后台报告整文件摘要失配，验证预占归零且原文件仍可下载。
	failed := awaitMultipartChecksumFailure(t, ctx, fixture.api, taskPath)
	if failed.ID != accepted.ID || failed.SizeBytes != fileSize || failed.LastError == nil || failed.LastError.Code != domain.UploadChecksumMismatchCode ||
		failed.ResultEntryID != nil || failed.ResolvedName != nil || failed.CompletedAt == nil || failed.Replacement == nil ||
		failed.Replacement.EntryID != fmt.Sprint(fixture.target.ID) || failed.Replacement.ExpectedRevision != fmt.Sprint(fixture.target.Revision) ||
		failed.Replacement.PublishedRevision != nil || failed.Replacement.ConflictingRevision != nil {
		t.Fatal("摘要失配终态未保留正确的原因或替换绑定", failed)
	}
	task, err := fixture.store.FindUserUploadTask(ctx, fixture.userID, taskID)
	if err != nil || task.Status != domain.UploadStatusFailed || task.ReservedBytes != 0 || task.PublishedObjectID != nil ||
		task.LeaseOwner != nil || task.LeaseUntil != nil {
		t.Fatal("失败任务未释放预占和租约，或保存了发布对象", err)
	}
	merged, err := fixture.store.FindUserStoredObject(ctx, fixture.userID, task.StagedObjectID)
	if err != nil || merged.Status != domain.StoredObjectStatusOrphaned || merged.VerifiedSHA256 != nil ||
		merged.Bucket != target.bucket || merged.ObjectKey != target.objectKey || merged.StorageVersionID == nil ||
		*merged.StorageVersionID == "" || *merged.StorageVersionID == "null" || merged.GCAfter == nil {
		t.Fatal("合并的未发布版本没有进入延迟回收", err)
	}
	fixture.checkQuota(t, ctx, originalSize, 0)
	if actual := readProtectedState(); actual != baseline {
		t.Fatalf("摘要失配改变了原文件、历史或事件，原 %s，现 %s", baseline, actual)
	}
	verifyReplacementDownload(t, ctx, fixture.backupSourceFixture, fixture.entryPath, fixture.target.Revision, fixture.originalObject, fixture.original)
	t.Logf("原文件 %d v%d 读回 %d B，SHA-256 %x；已用 %d B，释放 %d B，预占归零；历史保持空，游标保持 %d",
		fixture.target.ID, fixture.target.Revision, originalSize, sha256.Sum256(fixture.original), originalSize, fileSize, fixture.baselineSequence)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务退出失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
}
