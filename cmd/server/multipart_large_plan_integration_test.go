package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartLargePlanIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_LARGE_PLAN_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_LARGE_PLAN_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("大文件计划专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	api := fixture.api

	// 1. 循环使用同一合成数据块计算摘要，最后一次只写入剩余的 1 B。
	const fileSize uint64 = 1<<30 + 1
	sourceBlock := bytes.Repeat([]byte{0x31, 0x62, 0x93, 0xc4}, (64<<10)/4)
	digest := sha256.New()
	for remaining := fileSize; remaining > 0; {
		chunkSize := min(remaining, uint64(len(sourceBlock)))
		digest.Write(sourceBlock[:chunkSize])
		remaining -= chunkSize
	}
	sourceSHA256 := fmt.Sprintf("%x", digest.Sum(nil))

	// 2. 用正式创建接口核对模式与固定计划，根据响应计算尾片长度。
	const uploadsPath = "/api/v1/uploads"
	const fileName = "multipart-large-plan-fixture.bin"
	const expectedPartSize uint64 = 16 << 20
	const expectedPartCount = 65
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":%q}`,
		fixture.login.RootEntryID, fileName, fileSize, sourceSHA256)
	api.request(t, ctx, http.MethodPost, uploadsPath, "multipart-large-plan-fixture", strings.NewReader(body), http.StatusCreated, &created)
	plan := created.Data
	if created.RequestID == "" || plan.UploadMode != "MULTIPART" || plan.Task.UploadMode != "MULTIPART" ||
		plan.Task.ID == "" || plan.Task.Status != "CREATING" || plan.Task.SizeBytes != fileSize ||
		plan.Task.ParentID != fixture.login.RootEntryID || plan.Task.RequestedName != fileName || plan.Task.ResultEntryID != nil ||
		plan.PartSizeBytes != expectedPartSize || plan.PartCount != expectedPartCount {
		t.Fatal("正式创建接口未交付预期的大文件分片计划", plan)
	}
	tailSize := plan.Task.SizeBytes - plan.PartSizeBytes*uint64(plan.PartCount-1)
	if tailSize != 1 {
		t.Fatal("正式分片计划的尾片长度不符", tailSize)
	}

	// 3. 等待原任务绑定存储会话，通过回执和本人资料核对初始化及全额预占。
	taskPath := uploadsPath + "/" + plan.Task.ID
	ready := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	if ready.ID != plan.Task.ID || ready.UploadMode != plan.UploadMode || ready.SizeBytes != fileSize ||
		ready.ExpiresAt.IsZero() || !ready.ExpiresAt.Equal(plan.ExpiresAt) || ready.ResultEntryID != nil {
		t.Fatal("初始化后的原任务与创建快照不符", ready)
	}
	target := readServerMultipartPartTarget(t, ctx, fixture, ready)
	var receipts dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
	if receipts.RequestID == "" || receipts.Data.PartSizeBytes != plan.PartSizeBytes ||
		receipts.Data.PartCount != plan.PartCount || receipts.Data.Parts == nil || len(receipts.Data.Parts) != 0 {
		t.Fatal("新建会话未返回原计划及空回执", receipts.Data)
	}
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.RequestID == "" || profile.Data.User.ID != fixture.login.User.ID ||
		profile.Data.UsedBytes != 0 || profile.Data.ReservedBytes != fileSize {
		t.Fatal("新建大文件任务未全额预占", profile.Data.UsedBytes, profile.Data.ReservedBytes)
	}

	t.Logf("合成源 %d B、SHA-256 %s，任务 %s 从 CREATING 到 UPLOADING，标准片长 %d B、共 %d 片、尾片 %d B，回执 %d、已用 %d B、预占 %d B",
		fileSize, sourceSHA256, ready.ID, plan.PartSizeBytes, plan.PartCount, tailSize,
		len(receipts.Data.Parts), profile.Data.UsedBytes, profile.Data.ReservedBytes)

	t.Run("upload-first-part", func(t *testing.T) {
		// 1. 以创建摘要时使用的合成数据块填满第一片，向正式接口申请签名。
		const firstPartNumber = 1
		firstPart := bytes.Repeat(sourceBlock, int(plan.PartSizeBytes)/len(sourceBlock))
		if uint64(len(firstPart)) != plan.PartSizeBytes {
			t.Fatal("合成首片长度与计划不符", len(firstPart))
		}
		signedURL, checksum := target.signPart(t, ctx, api, firstPartNumber, firstPart)

		// 2. 把首片写入专用 MinIO，核对正式回执清单与实际 PUT 返回的 ETag 和摘要。
		etag := putMultipartProbePart(t, ctx, api.httpClient, signedURL, checksum, firstPart, http.StatusOK, "")
		wantPart := dto.UploadPartReceiptData{
			PartNumber: firstPartNumber, SizeBytes: plan.PartSizeBytes, ETag: etag, ChecksumSHA256: checksum,
		}
		var uploaded dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &uploaded)
		if uploaded.RequestID == "" || uploaded.Data.PartSizeBytes != plan.PartSizeBytes ||
			uploaded.Data.PartCount != plan.PartCount || len(uploaded.Data.Parts) != 1 || uploaded.Data.Parts[0] != wantPart {
			t.Fatal("正式回执没有交付已写入的唯一首片", uploaded.Data)
		}

		// 3. 原任务继续等待其余分片，配额仍维持写入首片前的全额预占。
		var current dto.UploadTaskResponse
		api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &current)
		if current.RequestID == "" || current.Data.ID != ready.ID || current.Data.Status != "UPLOADING" ||
			current.Data.SizeBytes != fileSize || current.Data.ResultEntryID != nil {
			t.Fatal("首片写入后的任务状态不符", current.Data)
		}
		var profileAfter dto.UserProfileResponse
		api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profileAfter)
		if profileAfter.RequestID == "" || profileAfter.Data != profile.Data {
			t.Fatal("首片写入后账号资料或预占发生变化", profileAfter.Data)
		}
		t.Logf("原任务 %s 首片 PUT 200，正式回执仅第 %d 片，%d B，SHA-256（Base64）%s，ETag %s，UPLOADING、已用 %d B、预占 %d B",
			current.Data.ID, wantPart.PartNumber, wantPart.SizeBytes, wantPart.ChecksumSHA256, wantPart.ETag,
			profileAfter.Data.UsedBytes, profileAfter.Data.ReservedBytes)
	})

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
}
