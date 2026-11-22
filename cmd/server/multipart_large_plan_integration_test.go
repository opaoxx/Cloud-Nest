package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartLargePlanIntegration(t *testing.T) {
	measureMemory := os.Getenv("CLOUDNEST_MULTIPART_LARGE_MEMORY_TEST") == "1"
	if measureMemory && runtime.GOOS != "windows" {
		t.Skip("大文件内存验收使用 Windows 进程计数器")
	}
	downloadUpload := os.Getenv("CLOUDNEST_MULTIPART_LARGE_DOWNLOAD_TEST") == "1"
	completeUpload := measureMemory || downloadUpload || os.Getenv("CLOUDNEST_MULTIPART_LARGE_COMPLETION_TEST") == "1"
	uploadAllParts := completeUpload || os.Getenv("CLOUDNEST_MULTIPART_LARGE_TRANSFER_TEST") == "1"
	if os.Getenv("CLOUDNEST_MULTIPART_LARGE_PLAN_TEST") != "1" && !uploadAllParts {
		t.Skip("按需启用 CLOUDNEST_MULTIPART_LARGE_PLAN_TEST、CLOUDNEST_MULTIPART_LARGE_TRANSFER_TEST、CLOUDNEST_MULTIPART_LARGE_COMPLETION_TEST、CLOUDNEST_MULTIPART_LARGE_DOWNLOAD_TEST 或 CLOUDNEST_MULTIPART_LARGE_MEMORY_TEST")
	}
	timeout := 90 * time.Second
	startMinIO := startIsolatedWebMinIO
	if uploadAllParts {
		timeout = 3 * time.Minute
		dataSize := "1536m"
		if completeUpload {
			// 为存储合并期间的分片及目标对象预留空间。
			dataSize = "2560m"
		}
		startMinIO = func(t *testing.T, ctx context.Context) string {
			t.Helper()
			return startIsolatedWebMinIOWithDataSize(t, ctx, dataSize)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	startServer := startBackupFixtureServer
	var backendPID int
	if measureMemory {
		startServer = func(t *testing.T, ctx context.Context, databaseName string) (backupSourceClient, func() (string, error)) {
			t.Helper()
			client, stop, pid := startIsolatedMultipartBackend(t, ctx, databaseName)
			backendPID = pid
			return client, stop
		}
	}
	fixture := prepareBackupSourceWithServer(t, ctx, startMinIO, startServer)
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
	sourceDigest := [sha256.Size]byte(digest.Sum(nil))
	sourceSHA256 := fmt.Sprintf("%x", sourceDigest)

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

	readPendingProfile := func(t *testing.T) dto.UserProfileData {
		t.Helper()
		var current dto.UploadTaskResponse
		api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &current)
		if current.RequestID == "" || current.Data.ID != ready.ID || current.Data.Status != "UPLOADING" ||
			current.Data.SizeBytes != fileSize || current.Data.ResultEntryID != nil {
			t.Fatal("分片写入后的原任务状态不符", current.Data)
		}
		var profileAfter dto.UserProfileResponse
		api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profileAfter)
		if profileAfter.RequestID == "" || profileAfter.Data != profile.Data {
			t.Fatal("分片写入后账号资料或预占发生变化", profileAfter.Data)
		}
		return profileAfter.Data
	}

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
		afterFirstPart := readPendingProfile(t)
		t.Logf("原任务 %s 首片 PUT 200，正式回执仅第 %d 片，%d B，SHA-256（Base64）%s，ETag %s，UPLOADING、已用 %d B、预占 %d B",
			ready.ID, wantPart.PartNumber, wantPart.SizeBytes, wantPart.ChecksumSHA256, wantPart.ETag,
			afterFirstPart.UsedBytes, afterFirstPart.ReservedBytes)

		if uploadAllParts {
			t.Run("upload-remaining-parts", func(t *testing.T) {
				receipts := verifyLargeMultipartRemainingParts(t, ctx, api, target, plan, firstPart, wantPart)
				afterAllParts := readPendingProfile(t)
				t.Logf("原任务 %s UPLOADING、已用 %d B、预占 %d B", ready.ID, afterAllParts.UsedBytes, afterAllParts.ReservedBytes)

				if completeUpload {
					t.Run("publish", func(t *testing.T) {
						var memoryBefore largeMultipartBackendMemory
						if measureMemory {
							memoryBefore = readLargeMultipartBackendMemory(t, ctx, backendPID)
						}
						// 1. 提交正式回执，并核对受理时保存的完成快照。
						accepted := submitMultipartCompletion(t, ctx, api, taskPath, ready, receipts...)
						observedAt := verifyMultipartCompletionSnapshot(t, ctx, fixture, receipts, ready, accepted)
						t.Logf("原任务 %s 返回 202 COMPLETING，%d 条完成快照逐项一致，核对时间 %s",
							accepted.ID, len(receipts), observedAt.Format(time.RFC3339Nano))

						// 2. 等待真实后台合并、计算整文件摘要并发布原任务。
						completed := awaitResumedMultipartPublication(t, ctx, api, taskPath, accepted)

						// 3. 核对唯一文件、已校验的对象版本和预占全额转入已用。
						publication := verifyResumedMultipartPublication(t, ctx, fixture, api, completed, sourceDigest)

						if measureMemory {
							memoryAfter := readLargeMultipartBackendMemory(t, ctx, backendPID)
							verifyLargeMultipartBackendMemory(t, backendPID, memoryBefore, memoryAfter)
						}
						if downloadUpload {
							t.Run("download", func(t *testing.T) {
								// 完整读取 1 GiB 正文单独使用更长的请求期限，并沿用隔离客户端的重定向规则。
								downloadAPI := api
								downloadClient := *api.httpClient
								downloadClient.Timeout = 45 * time.Second
								downloadAPI.httpClient = &downloadClient
								verifyResumedMultipartDownload(t, ctx, downloadAPI, publication, sourceDigest)
							})
						}
					})
				}
			})
		}
	})

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
}

func verifyLargeMultipartRemainingParts(t *testing.T, ctx context.Context, api backupSourceClient, target serverMultipartPartTarget,
	plan dto.CreateMultipartUploadData, partBuffer []byte, firstReceipt dto.UploadPartReceiptData) []dto.UploadPartReceiptData {
	t.Helper()
	// 1. 合成源按同一字节块循环，复用首片缓冲区，尾片只取剩余前缀。
	wantReceipts := make([]dto.UploadPartReceiptData, 0, plan.PartCount)
	wantReceipts = append(wantReceipts, firstReceipt)
	remaining := plan.Task.SizeBytes - firstReceipt.SizeBytes
	for partNumber := firstReceipt.PartNumber + 1; partNumber <= plan.PartCount; partNumber++ {
		partSize := min(plan.PartSizeBytes, remaining)
		partBody := partBuffer[:partSize]
		signedURL, checksum := target.signPart(t, ctx, api, partNumber, partBody)
		etag := putMultipartProbePart(t, ctx, api.httpClient, signedURL, checksum, partBody, http.StatusOK, "")
		wantReceipts = append(wantReceipts, dto.UploadPartReceiptData{
			PartNumber: partNumber, SizeBytes: partSize, ETag: etag, ChecksumSHA256: checksum,
		})
		remaining -= partSize
	}

	// 2. 用正式回执查询比对逐片 PUT 的结果，确保编号、长度及校验值按顺序一致。
	var uploaded dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, target.taskPath+"/parts", "", nil, http.StatusOK, &uploaded)
	if uploaded.RequestID == "" || uploaded.Data.PartSizeBytes != plan.PartSizeBytes ||
		uploaded.Data.PartCount != plan.PartCount || !slices.Equal(uploaded.Data.Parts, wantReceipts) {
		t.Fatal("正式回执与实际写入的完整分片清单不符", len(uploaded.Data.Parts))
	}

	// 3. 从服务端回执独立汇总字节数，并核对 1 B 尾片。
	var totalSize uint64
	for _, receipt := range uploaded.Data.Parts {
		totalSize += receipt.SizeBytes
	}
	tail := uploaded.Data.Parts[len(uploaded.Data.Parts)-1]
	if remaining != 0 || totalSize != plan.Task.SizeBytes || tail.SizeBytes != 1 {
		t.Fatal("完整回执的总字节数或尾片不符", totalSize, tail.SizeBytes, remaining)
	}
	t.Logf("全部 %d 片回执合计 %d B，尾片 %d 为 %d B，SHA-256（Base64）%s，ETag %s，复用 %d B 正文缓冲区",
		len(uploaded.Data.Parts), totalSize, tail.PartNumber, tail.SizeBytes, tail.ChecksumSHA256, tail.ETag, len(partBuffer))
	return uploaded.Data.Parts
}
