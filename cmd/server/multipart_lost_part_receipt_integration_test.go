package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	sourceDigest := sha256.Sum256(payload)
	const filename = "multipart-lost-receipt-fixture.bin"
	sourcePath := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(sourcePath, payload, 0600); err != nil {
		t.Fatal("保存合成源文件失败", err)
	}
	api := fixture.api
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, filename, fileSize, sourceDigest)
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
	readTaskAndProfile := func() (dto.UploadTaskData, dto.UserProfileData) {
		t.Helper()
		var task dto.UploadTaskResponse
		api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &task)
		var profile dto.UserProfileResponse
		api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
		if task.RequestID == "" || profile.RequestID == "" {
			t.Fatal("任务或资料响应缺少请求 ID")
		}
		return task.Data, profile.Data
	}
	task, profile := readTaskAndProfile()
	if !reflect.DeepEqual(task, readyTask) || profile.User.ID != fixture.login.User.ID ||
		profile.UsedBytes != 0 || profile.ReservedBytes != fileSize {
		t.Fatal("回执核对改变了原任务或预占配额")
	}
	t.Logf("原任务 %s 的本地回执仅第 1 片，存储回执有第 1、2 片，尾片 %d B，ETag %s，SHA-256 %s，待确认分片 %v，已用 %d B、预占 %d B",
		task.ID, remoteTail.SizeBytes, remoteTail.ETag, remoteTail.ChecksumSHA256, unconfirmed, profile.UsedBytes, profile.ReservedBytes)

	// 4. 重读合成源，按原计划重传待确认的尾片，并保存本次 PUT 回执。
	reopened, err := os.ReadFile(sourcePath)
	if err != nil || uint64(len(reopened)) != task.SizeBytes || sha256.Sum256(reopened) != sourceDigest {
		t.Fatal("重传前无法核对合成源", err)
	}
	retryNumber := unconfirmed[0]
	retrySize, err := domain.ExpectedMultipartUploadPartSize(task.SizeBytes, remote.Data.PartSizeBytes, remote.Data.PartCount, retryNumber)
	if err != nil || retrySize != remoteTail.SizeBytes {
		t.Fatal("原计划的待确认分片长度不符", retrySize, err)
	}
	offset := uint64(retryNumber-1) * remote.Data.PartSizeBytes
	retryPart := reopened[offset : offset+retrySize]
	retryURL, retryChecksum := target.signPart(t, ctx, api, retryNumber, retryPart)
	retryETag := putMultipartProbePart(t, ctx, api.httpClient, retryURL, retryChecksum, retryPart, http.StatusOK, "")
	localReceipts = append(localReceipts, dto.UploadPartReceiptData{
		PartNumber: retryNumber, SizeBytes: retrySize, ETag: retryETag, ChecksumSHA256: retryChecksum,
	})

	// 5. 读取原任务的最新回执，核对本地两片、任务和配额。
	var reconciled dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &reconciled)
	resumedTask, resumedProfile := readTaskAndProfile()
	if reconciled.RequestID == "" || reconciled.Data.PartSizeBytes != partSize || reconciled.Data.PartCount != 2 ||
		!slices.Equal(reconciled.Data.Parts, localReceipts) || !reflect.DeepEqual(resumedTask, task) ||
		!reflect.DeepEqual(resumedProfile, profile) {
		t.Fatal("重传后本地回执、存储回执、任务或配额不符", reconciled.Data)
	}
	t.Logf("原任务 %s 仅重传待确认的第 %d 片 %d B，保存 PUT 回执 ETag %s，两片本地回执与存储一致，已用 %d B、预占 %d B",
		resumedTask.ID, retryNumber, retrySize, retryETag, resumedProfile.UsedBytes, resumedProfile.ReservedBytes)

	// 6. 提交已核对的本地回执，等待后台发布并验证唯一条目和配额结算。
	accepted := submitResumedMultipartCompletion(t, ctx, api, taskPath, resumedTask, localReceipts[0], localReceipts[1])
	t.Logf("回执丢失重传后的原任务 %s 返回 202 COMPLETING", accepted.ID)
	completed := awaitResumedMultipartPublication(t, ctx, api, taskPath, accepted)
	verifyResumedMultipartPublication(t, ctx, fixture, api, completed, sourceDigest)

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
}
