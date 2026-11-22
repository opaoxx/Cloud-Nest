package main

import (
	"context"
	"crypto/sha256"
	"errors"
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

var errMultipartFixtureSourceDigestMismatch = errors.New("合成源的整文件 SHA-256 已变化")

func TestServerMultipartChangedSourceIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CHANGED_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_CHANGED_SOURCE_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("源变化专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])

	// 1. 创建两片任务并上传首片，记录源身份、原任务和账号配额。
	const partSize = domain.MaxSingleUploadSizeBytes
	const fileSize = partSize + 1
	const filename = "multipart-changed-source-fixture.bin"
	sourcePath := filepath.Join(t.TempDir(), filename)
	payload := make([]byte, fileSize)
	payload[0], payload[partSize] = 0x31, 0x32
	sourceDigest := sha256.Sum256(payload)
	if err := os.WriteFile(sourcePath, payload, 0600); err != nil {
		t.Fatal("保存临时合成源失败", err)
	}
	api := fixture.api
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, filename, fileSize, sourceDigest)
	api.request(t, ctx, http.MethodPost, "/api/v1/uploads", "multipart-changed-source-fixture", strings.NewReader(body), http.StatusCreated, &created)
	if created.RequestID == "" || created.Data.UploadMode != "MULTIPART" || created.Data.Task.Status != "CREATING" ||
		created.Data.PartSizeBytes != partSize || created.Data.PartCount != 2 {
		t.Fatal("合成任务没有取得预期两片计划", created.Data)
	}
	taskPath := "/api/v1/uploads/" + created.Data.Task.ID
	readyTask := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture, readyTask)
	firstPart := payload[:partSize]
	firstURL, firstChecksum := target.signPart(t, ctx, api, 1, firstPart)
	firstETag := putMultipartProbePart(t, ctx, api.httpClient, firstURL, firstChecksum, firstPart, http.StatusOK, "")
	firstReceipt := dto.UploadPartReceiptData{PartNumber: 1, SizeBytes: partSize, ETag: firstETag, ChecksumSHA256: firstChecksum}
	type uploadSnapshot struct {
		task    dto.UploadTaskData
		parts   dto.UploadPartsData
		profile dto.UserProfileData
	}
	readSnapshot := func() uploadSnapshot {
		t.Helper()
		var task dto.UploadTaskResponse
		api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &task)
		var parts dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &parts)
		var profile dto.UserProfileResponse
		api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
		return uploadSnapshot{task: task.Data, parts: parts.Data, profile: profile.Data}
	}
	before := readSnapshot()
	if !reflect.DeepEqual(before.task, readyTask) || before.task.SizeBytes != fileSize ||
		before.parts.PartSizeBytes != partSize || before.parts.PartCount != 2 || !slices.Equal(before.parts.Parts, []dto.UploadPartReceiptData{firstReceipt}) ||
		before.profile.User.ID != fixture.login.User.ID || before.profile.UsedBytes != 0 || before.profile.ReservedBytes != fileSize {
		t.Fatal("源变化前的任务、首片或配额不符")
	}

	// 2. 只改临时源的尾字节，保持长度不变，使用与正常续传相同的入口。
	sourceFile, err := os.OpenFile(sourcePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal("打开临时合成源失败", err)
	}
	_, writeErr := sourceFile.WriteAt([]byte{payload[partSize] ^ 1}, int64(partSize))
	closeErr := sourceFile.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("改写合成源最后一字节失败", writeErr, closeErr)
	}
	signAndPutCalls := 0
	_, err = resumeMultipartFixturePart(sourcePath, before.task.SizeBytes, sourceDigest, before.parts, 2,
		func([]byte) dto.UploadPartReceiptData {
			signAndPutCalls++
			return dto.UploadPartReceiptData{}
		})
	if !errors.Is(err, errMultipartFixtureSourceDigestMismatch) || signAndPutCalls != 0 {
		t.Fatal("同长度的源文件变化未在签发和 PUT 前拦截", err, signAndPutCalls)
	}

	// 3. 复查原任务、首片及账号配额，确认待补尾片仍然缺失。
	if after := readSnapshot(); !reflect.DeepEqual(after, before) {
		t.Fatal("源身份不匹配后任务、回执或配额发生变化", after)
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("原任务 %s 的合成源仍长 %d B，整文件 SHA-256 变化后签发和 PUT 入口调用 %d 次，仅保留首片，已用 %d B、预占 %d B",
		readyTask.ID, before.task.SizeBytes, signAndPutCalls, before.profile.UsedBytes, before.profile.ReservedBytes)
}

func resumeMultipartFixturePart(sourcePath string, expectedSize uint64, expectedDigest [sha256.Size]byte,
	plan dto.UploadPartsData, partNumber uint32, upload func([]byte) dto.UploadPartReceiptData) (dto.UploadPartReceiptData, error) {
	// 1. 重读当前文件，先核对大小，再核对原声明的整文件摘要。
	payload, err := os.ReadFile(sourcePath)
	if err != nil {
		return dto.UploadPartReceiptData{}, fmt.Errorf("重读合成源失败: %w", err)
	}
	if uint64(len(payload)) != expectedSize {
		return dto.UploadPartReceiptData{}, fmt.Errorf("合成源大小变化: %d B，原声明 %d B", len(payload), expectedSize)
	}
	if sha256.Sum256(payload) != expectedDigest {
		return dto.UploadPartReceiptData{}, errMultipartFixtureSourceDigestMismatch
	}

	// 2. 源身份通过后，按原计划切出指定分片，再交给签发和上传入口。
	size, err := domain.ExpectedMultipartUploadPartSize(expectedSize, plan.PartSizeBytes, plan.PartCount, partNumber)
	if err != nil {
		return dto.UploadPartReceiptData{}, err
	}
	offset := uint64(partNumber-1) * plan.PartSizeBytes
	return upload(payload[offset : offset+size]), nil
}
