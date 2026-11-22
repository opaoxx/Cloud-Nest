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
	api := fixture.api
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":"multipart-expired-url-fixture.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, fileSize, sha256.Sum256(payload))
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
	var task dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &task)
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if !reflect.DeepEqual(after, before) || task.RequestID == "" || !reflect.DeepEqual(task.Data, readyTask) ||
		!readyTask.ExpiresAt.After(time.Now()) || profile.Data.User.ID != fixture.login.User.ID ||
		profile.Data.UsedBytes != 0 || profile.Data.ReservedBytes != fileSize {
		t.Fatal("链接过期后任务、已确认分片或配额发生变化")
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("尾片签名到期时间 %s，原任务 %s 仍为 UPLOADING，仅有首片 %d B，已用 %d B、预占 %d B",
		expiredAt.Format(time.RFC3339), readyTask.ID, firstReceipt.SizeBytes, profile.Data.UsedBytes, profile.Data.ReservedBytes)
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
