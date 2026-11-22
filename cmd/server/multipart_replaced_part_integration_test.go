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
	sourceDigest := sha256.Sum256(payload)
	api := fixture.api
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	body := fmt.Sprintf(`{"parent_id":%q,"name":"multipart-replaced-part-fixture.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, fileSize, sourceDigest)
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
	readReceipts := func(expected ...dto.UploadPartReceiptData) []dto.UploadPartReceiptData {
		t.Helper()
		var receipts dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
		if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 ||
			!slices.Equal(receipts.Data.Parts, expected) {
			t.Fatal("正式分片清单与预期回执不符", receipts.Data)
		}
		return receipts.Data.Parts
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
	t.Logf("原任务 %s 的同编号尾片替换后只返回最新回执，长度 %d B，旧 ETag %s，新 ETag %s，旧 SHA-256 %s，新 SHA-256 %s，首片不变，已用 %d B、预占 %d B",
		readyTask.ID, replacement.SizeBytes, olderTail.ETag, replacement.ETag, olderTail.ChecksumSHA256, replacement.ChecksumSHA256,
		before.UsedBytes, before.ReservedBytes)

	// 4. 读取本轮完成核对前的账目，保留之后核对租约与写入的依据。
	type completionState struct {
		sessionID                 string
		leaseReleased             bool
		version, snapshots, files uint64
	}
	readCompletionState := func() completionState {
		t.Helper()
		var state completionState
		if err := fixture.pool.QueryRowContext(ctx, `SELECT task.storage_upload_id,
			task.lease_owner IS NULL AND task.lease_until IS NULL, task.lease_version,
			(SELECT COUNT(*) FROM upload_parts WHERE task_id=task.id),
			(SELECT COUNT(*) FROM file_entries WHERE user_id=task.user_id AND entry_type='FILE')
			FROM upload_tasks task WHERE id=? AND user_id=?`, readyTask.ID, fixture.login.User.ID).
			Scan(&state.sessionID, &state.leaseReleased, &state.version, &state.snapshots, &state.files); err != nil {
			t.Fatal("读取本轮完成核对状态失败", err)
		}
		return state
	}
	prior := readCompletionState()
	if prior.sessionID != target.uploadID || !prior.leaseReleased || prior.snapshots != 0 || prior.files != 0 {
		t.Fatal("提交旧回执前的原任务状态不符")
	}

	// 5. 用未变化的首片和真实被替换的旧尾片请求完成，仅第 2 片应被指出。
	completeBody := fmt.Sprintf(`{"parts":[{"part_number":1,"etag":%q},{"part_number":2,"etag":%q}]}`,
		firstReceipt.ETag, olderTail.ETag)
	var rejected dto.ErrorResponse
	api.request(t, ctx, http.MethodPost, taskPath+"/complete", "", strings.NewReader(completeBody), http.StatusConflict, &rejected)
	if rejected.RequestID == "" || rejected.Error.Code != "UPLOAD_PARTS_MISMATCH" ||
		!reflect.DeepEqual(rejected.Error.Details, map[string]any{"part_numbers": []any{float64(2)}}) {
		t.Fatal("完成接口未仅指出已替换的尾片", rejected.Error)
	}

	// 6. 核对本次核对租约已释放，任务仍可上传，回执及配额没有被推进。
	after := readCompletionState()
	expected := prior
	expected.version++ // 领取核对租约会增加版本，释放后避免旧持有者再次写入。
	if after != expected {
		t.Fatal("旧尾片回执导致意外的会话、租约或发布状态", after)
	}
	var afterTask dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &afterTask)
	expectedTask := current.Data
	expectedTask.UpdatedAt = afterTask.Data.UpdatedAt // 领取和释放租约会更新任务时间。
	if afterTask.RequestID == "" || afterTask.Data.UpdatedAt.Before(current.Data.UpdatedAt) ||
		!reflect.DeepEqual(afterTask.Data, expectedTask) || !reflect.DeepEqual(readProfile(), before) {
		t.Fatal("拒绝旧尾片回执改变了原任务或预占配额", afterTask.Data)
	}
	latest := readReceipts(firstReceipt, replacement)
	t.Logf("原任务 %s 提交旧尾片回执返回 409 UPLOAD_PARTS_MISMATCH，只指出第 2 片，核对租约版本 %d 到 %d 后释放，任务仍为 %s，完成快照 %d、文件 %d、预占 %d B",
		readyTask.ID, prior.version, after.version, afterTask.Data.Status, after.snapshots, after.files, before.ReservedBytes)

	// 7. 以正式查询的最新回执重新提交原任务，验证后台发布及配额结算。
	accepted := submitMultipartCompletion(t, ctx, api, taskPath, afterTask.Data, latest...)
	t.Logf("替换尾片后的原任务 %s 使用最新回执返回 202 COMPLETING", accepted.ID)
	completed := awaitResumedMultipartPublication(t, ctx, api, taskPath, accepted)
	publication := verifyResumedMultipartPublication(t, ctx, fixture, api, completed, sourceDigest)

	// 8. 通过正式授权下载，验证已发布版本的完整正文包含替换后的尾片。
	verifyResumedMultipartDownload(t, ctx, api, publication, sourceDigest)

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
}
