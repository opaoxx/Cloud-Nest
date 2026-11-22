package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartSizeLimitIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_SIZE_LIMIT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_SIZE_LIMIT_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("文件上限专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	api := fixture.api

	// 1. 创建正好 50 GiB 的任务，按创建响应中的数值计算最后一片长度。
	const uploadsPath = "/api/v1/uploads"
	const sizeLimit uint64 = 50 << 30
	const standardPartSize uint64 = 16 << 20
	const expectedPartCount = 3200
	// 本用例只验证声明阶段，使用格式有效的合成摘要。
	declaredDigest := strings.Repeat("a5", 32)
	declare := func(name string, size uint64) *strings.Reader {
		return strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":%q}`,
			fixture.login.RootEntryID, name, size, declaredDigest))
	}
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	api.request(t, ctx, http.MethodPost, uploadsPath, "multipart-size-limit",
		declare("at-size-limit.bin", sizeLimit), http.StatusCreated, &created)
	plan := created.Data
	if created.RequestID == "" || plan.UploadMode != "MULTIPART" || plan.Task.UploadMode != plan.UploadMode ||
		plan.Task.ID == "" || plan.Task.Status != "CREATING" || plan.Task.ParentID != fixture.login.RootEntryID ||
		plan.Task.SizeBytes != sizeLimit || plan.PartSizeBytes != standardPartSize || plan.PartCount != expectedPartCount {
		t.Fatal("正式接口未交付 50 GiB 的固定分片计划", plan)
	}
	lastPartSize := plan.Task.SizeBytes - uint64(plan.PartCount-1)*plan.PartSizeBytes
	if lastPartSize != standardPartSize {
		t.Fatal("恰好整除的尾片应保留完整片长", lastPartSize)
	}

	// 2. 等待后台绑定隔离会话，核对实际保存的计划、空回执与全额预占。
	taskPath := uploadsPath + "/" + plan.Task.ID
	ready := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	if ready.ID != plan.Task.ID || ready.UploadMode != plan.UploadMode || ready.SizeBytes != sizeLimit ||
		!ready.ExpiresAt.Equal(plan.ExpiresAt) || ready.ResultEntryID != nil {
		t.Fatal("存储会话就绪后原任务与创建声明不符", ready)
	}
	readServerMultipartPartTarget(t, ctx, fixture, ready)
	var receipts dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
	if receipts.RequestID == "" || receipts.Data.PartSizeBytes != plan.PartSizeBytes ||
		receipts.Data.PartCount != plan.PartCount || receipts.Data.Parts == nil || len(receipts.Data.Parts) != 0 {
		t.Fatal("上限任务未返回预期计划和空回执", receipts.Data)
	}
	var before dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &before)
	if before.RequestID == "" || before.Data.User.ID != fixture.login.User.ID ||
		before.Data.UsedBytes != 0 || before.Data.ReservedBytes != sizeLimit {
		t.Fatal("上限任务未获得全额预占", before.Data)
	}

	// 3. 提交超过上限 1 B 的声明，通过资料接口和只读计数确认拒绝没有写入。
	var rejected dto.ErrorResponse
	api.request(t, ctx, http.MethodPost, uploadsPath, "multipart-over-size-limit",
		declare("over-size-limit.bin", sizeLimit+1), http.StatusRequestEntityTooLarge, &rejected)
	if rejected.RequestID == "" || rejected.Error.Code != "FILE_TOO_LARGE" ||
		len(rejected.Error.Details) != 2 || rejected.Error.Details["field"] != "size_bytes" ||
		rejected.Error.Details["max_size_bytes"] != float64(sizeLimit) {
		t.Fatal("超过上限 1 B 未返回约定的实际字节上限", rejected)
	}
	var after dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &after)
	if after.RequestID == "" || after.Data != before.Data {
		t.Fatal("超限请求改变了账号配额", before.Data, after.Data)
	}
	var tasks, objects, snapshots uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=users.id),
		(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=users.id),
		(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id AND operation=?)
		FROM users WHERE id=?`, domain.CreateUploadOperation, fixture.login.User.ID).
		Scan(&tasks, &objects, &snapshots); err != nil || tasks != 1 || objects != 1 || snapshots != 1 {
		t.Fatal("超限请求额外登记了任务、暂存对象或幂等快照", err, tasks, objects, snapshots)
	}

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("声明 %d B 创建任务 %s，%d 片、片长 %d B、尾片 %d B，UPLOADING、回执 0；声明 %d B 返回 413 FILE_TOO_LARGE，上限 %d B，任务、暂存对象及幂等快照各 %d 条，已用 %d B、预占 %d B",
		sizeLimit, ready.ID, plan.PartCount, plan.PartSizeBytes, lastPartSize, sizeLimit+1, sizeLimit, tasks,
		after.Data.UsedBytes, after.Data.ReservedBytes)
}
