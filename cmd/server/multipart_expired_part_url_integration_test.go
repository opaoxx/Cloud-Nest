package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartExpiredPartURLIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_EXPIRED_PART_URL_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_EXPIRED_PART_URL_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("签名过期专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])

	// 1. 创建两片合成任务，通过正式签发上传首片并记录回执。
	const partSize = domain.MaxSingleUploadSizeBytes
	const fileSize = partSize + 1
	payload := make([]byte, fileSize)
	payload[0], payload[partSize] = 0x31, 0x32
	sourceDigest := sha256.Sum256(payload)
	sourcePath := filepath.Join(t.TempDir(), "multipart-expired-url-fixture.bin")
	if err := os.WriteFile(sourcePath, payload, 0600); err != nil {
		t.Fatal("保存合成源文件失败", err)
	}
	api := fixture.api
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":"multipart-expired-url-fixture.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, fileSize, sourceDigest)
	api.request(t, ctx, http.MethodPost, "/api/v1/uploads", "multipart-expired-url-fixture", strings.NewReader(body), http.StatusCreated, &created)
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
	readParts := func() dto.UploadPartsData {
		t.Helper()
		var receipts dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
		if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 {
			t.Fatal("正式回执查询未返回原分片计划")
		}
		return receipts.Data
	}
	before := readParts()
	if !slices.Equal(before.Parts, []dto.UploadPartReceiptData{firstReceipt}) {
		t.Fatal("签名过期前的首片回执不符", before.Parts)
	}

	// 2. 在测试侧用正式存储适配器为缺失尾片签发短期 URL，等待该 URL 到期。
	tail := payload[partSize:]
	expiredAt := verifyExpiredMultipartTailURL(t, ctx, api.httpClient, target, tail)

	// 3. 在任务仍有效时回读原分片和配额，确认过期 PUT 未产生尾片。
	after := readParts()
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
	if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(task, readyTask) ||
		!readyTask.ExpiresAt.After(time.Now()) || profile.User.ID != fixture.login.User.ID ||
		profile.UsedBytes != 0 || profile.ReservedBytes != fileSize {
		t.Fatal("链接过期后任务、已确认分片或配额发生变化")
	}
	t.Logf("尾片签名到期时间 %s，原任务 %s 仍为 UPLOADING，仅有首片 %d B，已用 %d B、预占 %d B",
		expiredAt.Format(time.RFC3339), task.ID, firstReceipt.SizeBytes, profile.UsedBytes, profile.ReservedBytes)

	// 4. 核对源身份，用原任务为已确认缺失的尾片重新签发，不申请任务续期。
	reopened, err := os.ReadFile(sourcePath)
	if err != nil || uint64(len(reopened)) != task.SizeBytes || sha256.Sum256(reopened) != sourceDigest {
		t.Fatal("重新签发前无法核对合成源", err)
	}
	missingNumber := after.PartCount // 原两片计划中已经确认只有第 1 片。
	fullTail := reopened[after.PartSizeBytes:]
	newURL, newChecksum := target.signPart(t, ctx, api, missingNumber, fullTail)
	query := newURL.Query()
	signedAt, err := time.Parse("20060102T150405Z", query.Get("X-Amz-Date"))
	if err != nil || !signedAt.After(expiredAt) || query.Get("X-Amz-Expires") != "300" {
		t.Fatal("正式签发未在原链接过期后交付新的五分钟地址", err)
	}
	newETag := putMultipartProbePart(t, ctx, api.httpClient, newURL, newChecksum, fullTail, http.StatusOK, "")

	// 5. 核对两片回执和原任务期限，预占留待正式完成时结算。
	tailReceipt := dto.UploadPartReceiptData{
		PartNumber: missingNumber, SizeBytes: uint64(len(fullTail)), ETag: newETag, ChecksumSHA256: newChecksum,
	}
	resumed := readParts()
	resumedTask, resumedProfile := readTaskAndProfile()
	if !slices.Equal(resumed.Parts, []dto.UploadPartReceiptData{firstReceipt, tailReceipt}) ||
		!reflect.DeepEqual(resumedTask, task) || !reflect.DeepEqual(resumedProfile, profile) {
		t.Fatal("重新签发补传后的回执、任务或配额不符")
	}
	t.Logf("原任务 %s 正式重新签发后仅补传第 %d 片 %d B，ETag %s，SHA-256 %s，首片回执和任务期限保持原值，已用 %d B、预占 %d B",
		resumedTask.ID, tailReceipt.PartNumber, tailReceipt.SizeBytes, tailReceipt.ETag, tailReceipt.ChecksumSHA256,
		resumedProfile.UsedBytes, resumedProfile.ReservedBytes)

	// 6. 提交重签补传后的两片回执，核对正式后台发布结果和配额结算。
	accepted := submitResumedMultipartCompletion(t, ctx, api, taskPath, resumedTask, resumed.Parts[0], resumed.Parts[1])
	t.Logf("重签补传后的原任务 %s 返回 202 COMPLETING", accepted.ID)
	completed := awaitResumedMultipartPublication(t, ctx, api, taskPath, accepted)
	verifyResumedMultipartPublication(t, ctx, fixture, api, completed, sourceDigest)

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
}

func verifyExpiredMultipartTailURL(t *testing.T, ctx context.Context, httpClient *http.Client, target serverMultipartPartTarget, tail []byte) time.Time {
	t.Helper()
	settings, err := config.LoadStorageConfig()
	if err != nil || settings.Bucket != target.bucket {
		t.Fatal("过期签名未使用本轮隔离存储配置", err)
	}
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal("创建隔离存储签名客户端失败", err)
	}
	defer client.Close()

	// 1. 仅给测试侧签发调用传入较短期限，任务仍使用数据库原截止时间。
	checksum := multipartProbeSHA256(tail)
	const tailNumber = 2
	shortDeadline := time.Now().Add(4 * time.Second)
	link, err := client.SignUploadPartURL(ctx, target.objectKey, target.uploadID, tailNumber, uint64(len(tail)), checksum, shortDeadline)
	if err != nil || link == nil || link.ExpiresAt.After(shortDeadline) ||
		!link.ExpiresAt.After(time.Now()) || !link.ExpiresAt.Before(target.taskExpiration) {
		t.Fatal("无法取得先于原任务到期的隔离尾片链接", err)
	}
	signed, err := url.Parse(link.URL)
	if err != nil || signed.User != nil || signed.Scheme+"://"+signed.Host != os.Getenv("CLOUDNEST_S3_PUBLIC_ENDPOINT") ||
		signed.Hostname() != "127.0.0.1" || signed.Path != "/"+target.bucket+"/"+target.objectKey ||
		signed.Query().Get("uploadId") != target.uploadID || signed.Query().Get("partNumber") != "2" {
		t.Fatal("尾片签名未绑定在本轮独占回环会话")
	}
	wait := time.NewTimer(time.Until(link.ExpiresAt) + time.Second)
	defer wait.Stop()
	select {
	case <-wait.C:
	case <-ctx.Done():
		t.Fatal("等待隔离签名到期失败", ctx.Err())
	}

	// 2. 按签发时绑定的长度和摘要 PUT，校验存储明确以签名过期拒绝。
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, signed.String(), bytes.NewReader(tail))
	if err != nil {
		t.Fatal("构造隔离尾片 PUT 失败")
	}
	request.Header.Set(multipartProbeChecksumHeader, checksum)
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal("请求已过期的隔离签名失败")
	}
	defer response.Body.Close()
	var rejection struct{ Code, Message string }
	if err := xml.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&rejection); err != nil ||
		response.StatusCode != http.StatusForbidden || rejection.Code != "AccessDenied" ||
		rejection.Message != "Request has expired" || response.Header.Get("ETag") != "" {
		t.Fatalf("存储未明确拒绝过期尾片签名，HTTP %d，错误码 %s，解析错误 %v", response.StatusCode, rejection.Code, err)
	}
	t.Logf("尾片原签名 PUT 返回 403 AccessDenied，原因 Request has expired，未取得 ETag")
	return link.ExpiresAt
}
