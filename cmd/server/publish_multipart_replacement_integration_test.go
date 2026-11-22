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

func TestServerPublishMultipartUploadReplacementIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_PUBLISH_MULTIPART_REPLACEMENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_PUBLISH_MULTIPART_REPLACEMENT_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 创建替换任务，等待后台绑定分片会话。
	fixture := prepareReplacementPublicationFixture(t, ctx)
	t.Logf("分片替换发布专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	const partSize = domain.MaxSingleUploadSizeBytes
	payload := make([]byte, partSize+1)
	copy(payload, "CloudNest multipart replacement fixture")
	payload[partSize-1], payload[partSize] = 0x31, 0x32
	digest := sha256.Sum256(payload)
	var response struct {
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	body := fmt.Sprintf(`{"expected_revision":"%d","size_bytes":%d,"sha256":"%x"}`, fixture.target.Revision, len(payload), digest)
	fixture.api.request(t, ctx, http.MethodPost, fixture.entryPath+"/content-replacement", "multipart-replacement",
		strings.NewReader(body), http.StatusCreated, &response)
	created, err := dto.RestoreCreateMultipartUploadReplacementData(response.Data)
	if err != nil || response.RequestID == "" || created.PartSizeBytes != partSize || created.PartCount != 2 {
		t.Fatal("正式创建没有返回两片替换计划", err)
	}
	taskID := backupFixtureID(t, created.Task.ID)
	if err := dto.ValidateCreateUploadReplacementSnapshot(response.Data, taskID, fixture.target.ID, fixture.target.Revision, uint64(len(payload))); err != nil {
		t.Fatal("创建回执未绑定原文件和版本", err)
	}
	taskPath := "/api/v1/uploads/" + created.Task.ID
	ready := waitForCreateUploadInitialization(t, ctx, fixture.api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture.backupSourceFixture, ready)
	fixture.checkQuota(t, ctx, uint64(len(fixture.original)), uint64(len(payload)))

	// 2. 上传两片，查回回执并提交完成。
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
	if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 ||
		!slices.Equal(receipts.Data.Parts, received) {
		t.Fatal("分片编号、字节数、摘要或 ETag 与正式回执不符", receipts.Data)
	}
	accepted := submitMultipartCompletion(t, ctx, fixture.api, taskPath, ready, receipts.Data.Parts...)
	completed := awaitResumedMultipartPublication(t, ctx, fixture.api, taskPath, accepted)
	if completed.Replacement == nil || completed.Replacement.PublishedRevision == nil ||
		*completed.Replacement.PublishedRevision != fmt.Sprint(fixture.target.Revision+1) {
		t.Fatal("正式任务查询未交付替换后的发布版本", completed)
	}

	// 3. 核对合并对象仍使用原会话对象键，复用新旧内容和结算断言。
	published, err := fixture.store.FindUserUploadTask(ctx, fixture.userID, taskID)
	if err != nil || published.PublishedObjectID == nil {
		t.Fatal("完成任务未引用合并对象", err)
	}
	object, err := fixture.store.FindUserStoredObject(ctx, fixture.userID, *published.PublishedObjectID)
	if err != nil || object.Bucket != target.bucket || object.ObjectKey != target.objectKey ||
		object.StorageVersionID == nil || *object.StorageVersionID == "" || *object.StorageVersionID == "null" {
		t.Fatal("发布对象未绑定本轮合并版本", err)
	}
	var frozenParts int
	if err := fixture.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_parts WHERE task_id=?", taskID).Scan(&frozenParts); err != nil || frozenParts != 2 {
		t.Fatal("合并后的两片完成回执未持久化", err)
	}
	t.Logf("同一任务 %d 的两片分别为 %d B、%d B，正式回执已核对，合并对象 %d，版本 %s",
		taskID, received[0].SizeBytes, received[1].SizeBytes, object.ID, *object.StorageVersionID)
	fixture.verifyPublished(t, ctx, published, payload)
}
