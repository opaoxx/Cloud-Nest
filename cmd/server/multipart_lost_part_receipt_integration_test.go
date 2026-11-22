package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartLostPartReceiptIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_LOST_PART_RECEIPT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_LOST_PART_RECEIPT_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("回执丢失专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])

	// 1. 完整上传两片，模拟客户端只保存首片回执。
	const partSize = domain.MaxSingleUploadSizeBytes
	const fileSize = partSize + 1
	payload := make([]byte, fileSize)
	payload[0], payload[partSize] = 0x31, 0x32
	api := fixture.api
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":"multipart-lost-receipt-fixture.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, fileSize, sha256.Sum256(payload))
	api.request(t, ctx, http.MethodPost, "/api/v1/uploads", "multipart-lost-receipt-fixture", strings.NewReader(body), http.StatusCreated, &created)
	if created.RequestID == "" || created.Data.UploadMode != "MULTIPART" || created.Data.Task.Status != "CREATING" ||
		created.Data.PartSizeBytes != partSize || created.Data.PartCount != 2 {
		t.Fatal("合成任务没有取得预期的两片计划", created.Data)
	}
	taskPath := "/api/v1/uploads/" + created.Data.Task.ID
	readyTask := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture, readyTask)
	firstPart := payload[:partSize]
	firstURL, firstChecksum := target.signPart(t, ctx, api, 1, firstPart)
	firstETag := putMultipartProbePart(t, ctx, api.httpClient, firstURL, firstChecksum, firstPart, http.StatusOK, "")
	firstReceipt := dto.UploadPartReceiptData{PartNumber: 1, SizeBytes: partSize, ETag: firstETag, ChecksumSHA256: firstChecksum}
	localReceipts := []dto.UploadPartReceiptData{firstReceipt}

	tail := payload[partSize:]
	tailURL, tailChecksum := target.signPart(t, ctx, api, 2, tail)
	// 确认尾片 PUT 成功，故意省略将返回回执记入客户端快照的步骤。
	putMultipartProbePart(t, ctx, api.httpClient, tailURL, tailChecksum, tail, http.StatusOK, "")

	// 2. 通过原任务查询存储侧的两片回执。
	var remote dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &remote)
	if remote.RequestID == "" || remote.Data.PartSizeBytes != partSize || remote.Data.PartCount != 2 ||
		len(remote.Data.Parts) != 2 || remote.Data.Parts[0] != firstReceipt {
		t.Fatal("原任务未交付完整的存储侧分片计划及首片回执", remote.Data)
	}
	remoteTail := remote.Data.Parts[1]
	if remoteTail.PartNumber != 2 || remoteTail.SizeBytes != uint64(len(tail)) ||
		remoteTail.ETag == "" || remoteTail.ChecksumSHA256 != tailChecksum {
		t.Fatal("存储侧未找到已上传尾片的回执", remoteTail)
	}

	// 3. 比对两份记录，将缺少本地回执的尾片列为待确认。
	var unconfirmed []uint32
	for _, storedReceipt := range remote.Data.Parts {
		if !slices.Contains(localReceipts, storedReceipt) {
			unconfirmed = append(unconfirmed, storedReceipt.PartNumber)
		}
	}
	if !slices.Equal(unconfirmed, []uint32{2}) {
		t.Fatal("应只有第 2 片待确认", unconfirmed)
	}
	var task dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &task)
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if task.RequestID == "" || profile.RequestID == "" || !reflect.DeepEqual(task.Data, readyTask) ||
		profile.Data.User.ID != fixture.login.User.ID || profile.Data.UsedBytes != 0 || profile.Data.ReservedBytes != fileSize {
		t.Fatal("回执核对改变了原任务或预占配额")
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("原任务 %s 的本地回执仅第 1 片，存储回执有第 1、2 片，尾片 %d B，ETag %s，SHA-256 %s，待确认分片 %v，已用 %d B、预占 %d B",
		task.Data.ID, remoteTail.SizeBytes, remoteTail.ETag, remoteTail.ChecksumSHA256, unconfirmed, profile.Data.UsedBytes, profile.Data.ReservedBytes)
}
