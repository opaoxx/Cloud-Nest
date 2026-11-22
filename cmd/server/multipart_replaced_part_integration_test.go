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

func TestServerMultipartReplacedPartIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_REPLACED_PART_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_REPLACED_PART_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("分片替换专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])

	// 1. 创建两片任务，上传首片和初始尾片，读取两片回执。
	const partSize = domain.MaxSingleUploadSizeBytes
	const fileSize = partSize + 1
	payload := make([]byte, fileSize)
	payload[0], payload[partSize] = 0x31, 0x32
	api := fixture.api
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":"multipart-replaced-part-fixture.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, fileSize, sha256.Sum256(payload))
	api.request(t, ctx, http.MethodPost, "/api/v1/uploads", "multipart-replaced-part-fixture", strings.NewReader(body), http.StatusCreated, &created)
	if created.RequestID == "" || created.Data.UploadMode != "MULTIPART" || created.Data.Task.Status != "CREATING" ||
		created.Data.PartSizeBytes != partSize || created.Data.PartCount != 2 {
		t.Fatal("合成任务没有取得预期两片计划", created.Data)
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
	readReceipts := func(expected ...dto.UploadPartReceiptData) {
		t.Helper()
		var receipts dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
		if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 ||
			!slices.Equal(receipts.Data.Parts, expected) {
			t.Fatal("正式分片清单与预期回执不符", receipts.Data)
		}
	}
	readProfile := func() dto.UserProfileData {
		t.Helper()
		var profile dto.UserProfileResponse
		api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
		if profile.RequestID == "" {
			t.Fatal("账号资料响应缺少请求 ID")
		}
		return profile.Data
	}
	firstReceipt := putPart(1, payload[:partSize])
	olderTail := putPart(2, []byte{0x33})
	readReceipts(firstReceipt, olderTail)
	before := readProfile()
	if before.UsedBytes != 0 || before.ReservedBytes != fileSize {
		t.Fatal("尾片替换前配额不符", before.UsedBytes, before.ReservedBytes)
	}

	// 2. 对同一个会话和尾片编号签发并上传等长的新内容。
	replacement := putPart(2, payload[partSize:])
	if replacement.SizeBytes != olderTail.SizeBytes || replacement.ETag == olderTail.ETag ||
		replacement.ChecksumSHA256 == olderTail.ChecksumSHA256 {
		t.Fatal("替换尾片须保留长度并取得不同的 ETag 和摘要", replacement)
	}

	// 3. 重新读取原任务，只能看到最新尾片，首片和配额保持原值。
	readReceipts(firstReceipt, replacement)
	var current dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &current)
	if current.RequestID == "" || !reflect.DeepEqual(current.Data, readyTask) || !reflect.DeepEqual(readProfile(), before) {
		t.Fatal("替换尾片改变了原任务或预占配额")
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("原任务 %s 的同编号尾片替换后只返回最新回执，长度 %d B，旧 ETag %s，新 ETag %s，旧 SHA-256 %s，新 SHA-256 %s，首片不变，已用 %d B、预占 %d B",
		readyTask.ID, replacement.SizeBytes, olderTail.ETag, replacement.ETag, olderTail.ChecksumSHA256, replacement.ChecksumSHA256,
		before.UsedBytes, before.ReservedBytes)
}
