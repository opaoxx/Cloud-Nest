package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerSingleUploadTempSpaceIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SINGLE_UPLOAD_TEMP_SPACE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_SINGLE_UPLOAD_TEMP_SPACE_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 用实际共享预算模拟暂存空间紧张，并通过正式接口创建 SINGLE 任务。
	const capacity = 128 << 20
	const availableBytes = 3
	const payload = "test"
	uploadSpace := service.NewUploadTempSpace()
	release, err := uploadSpace.Reserve(ctx, capacity-availableBytes)
	if err != nil {
		t.Fatal("无法预占隔离实例的暂存预算", err)
	}
	defer release()
	fixture := prepareBackupSourceWithServer(t, ctx, startIsolatedWebMinIO,
		func(t *testing.T, ctx context.Context, databaseName string) (backupSourceClient, func() (string, error)) {
			t.Helper()
			return startBackupFixtureServerWithRun(t, ctx, databaseName, func(ctx context.Context, logger *slog.Logger) error {
				return runWithServerDependencies(ctx, logger, startServerPurgeRootWorker, uploadSpace)
			})
		})
	t.Logf("暂存预算专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	api := fixture.api
	const uploadsPath = "/api/v1/uploads"
	declaration := fmt.Sprintf(`{"parent_id":%q,"name":"temp-budget-fixture.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, len(payload), sha256.Sum256([]byte(payload)))
	var created dto.CreateUploadResponse
	api.request(t, ctx, http.MethodPost, uploadsPath, "temp-budget-fixture", strings.NewReader(declaration), http.StatusCreated, &created)
	taskPath := uploadsPath + "/" + created.Data.Task.ID
	if created.RequestID == "" || created.Data.UploadMode != "SINGLE" || created.Data.Task.ID == "" ||
		created.Data.Task.Status != "UPLOADING" || created.Data.Task.SizeBytes != uint64(len(payload)) ||
		created.Data.ContentPath != taskPath+"/content" {
		t.Fatal("暂存预算夹具未取得预期的 SINGLE 任务", created.Data)
	}

	// 2. 等待发送正文许可时，服务应直接拒绝本次接收。
	verifySingleUploadTempSpaceResponse(t, ctx, api, created.Data.ContentPath, len(payload))

	// 3. 重读任务及资源，核对没有领取租约、暂存文件、对象版本或发布条目。
	var after dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &after)
	if after.RequestID == "" || after.Data != created.Data.Task {
		t.Fatal("空间不足后的原任务快照发生变化", after.Data)
	}
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.RequestID == "" || profile.Data.User.ID != fixture.login.User.ID ||
		profile.Data.UsedBytes != 0 || profile.Data.ReservedBytes != uint64(len(payload)) {
		t.Fatal("空间不足后的账号配额不符", profile.Data)
	}
	var reserved, leaseVersion, entries uint64
	var leaseFree, versionAbsent bool
	var objectStatus string
	if err := fixture.pool.QueryRowContext(ctx, `SELECT task.reserved_bytes,task.lease_version,
		task.lease_owner IS NULL AND task.lease_until IS NULL,obj.status,obj.storage_version_id IS NULL,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=task.user_id AND entry_type='FILE')
		FROM upload_tasks task JOIN stored_objects obj ON obj.id=task.staged_object_id AND obj.owner_user_id=task.user_id
		WHERE task.id=? AND task.user_id=? AND obj.bucket=?`, after.Data.ID, fixture.login.User.ID, fixture.bucket.name).
		Scan(&reserved, &leaseVersion, &leaseFree, &objectStatus, &versionAbsent, &entries); err != nil ||
		reserved != uint64(len(payload)) || leaseVersion != 0 || !leaseFree || objectStatus != "STAGING" || !versionAbsent || entries != 0 {
		t.Fatal("空间不足后的租约、对象记录或发布状态不符", err, reserved, leaseVersion, leaseFree, objectStatus, versionAbsent, entries)
	}
	if files, err := os.ReadDir(os.Getenv("CLOUDNEST_TEMP_DIR")); err != nil || len(files) != 0 {
		t.Fatal("空间不足时专用暂存目录未保持为空", len(files), err)
	}
	for object := range fixture.bucket.client.ListObjects(ctx, fixture.bucket.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		t.Fatal("空间不足时专用桶未保持为空", object.Err)
	}
	t.Logf("暂存可用 %d B、声明 %d B，未发送正文即收到 507 TEMP_SPACE_EXHAUSTED，任务 %s 保持 UPLOADING，租约版本 %d，临时文件、桶版本及发布条目均为 0，已用 %d B、预占 %d B",
		availableBytes, len(payload), after.Data.ID, leaseVersion, profile.Data.UsedBytes, profile.Data.ReservedBytes)

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
}

func verifySingleUploadTempSpaceResponse(t *testing.T, ctx context.Context, api backupSourceClient, contentPath string, sizeBytes int) {
	t.Helper()
	// 1. 只向夹具的回环地址发送请求头，正文等待服务许可。
	address := strings.TrimPrefix(api.baseURL, "http://")
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	socket, err := (&net.Dialer{}).DialContext(probeContext, "tcp", address)
	if err != nil {
		t.Fatal("无法连接隔离上传服务", err)
	}
	defer socket.Close()
	deadline, _ := probeContext.Deadline()
	if err := socket.SetDeadline(deadline); err != nil {
		t.Fatal("无法设置隔离上传请求期限", err)
	}
	if _, err := fmt.Fprintf(socket, "PUT %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n"+
		"Content-Type: application/octet-stream\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n",
		contentPath, address, api.accessToken, sizeBytes); err != nil {
		t.Fatal("无法发送隔离接收请求头", err)
	}

	// 2. 第一条响应必须是固定的容量错误，并带有追踪 ID 和连接收尾。
	response, err := http.ReadResponse(bufio.NewReader(socket), &http.Request{Method: http.MethodPut})
	if err != nil {
		t.Fatal("读取隔离容量拒绝响应失败", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusInsufficientStorage || !response.Close {
		t.Fatal("首个响应未直接拒绝接收", response.StatusCode, response.Close)
	}
	var rejected dto.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&rejected); err != nil {
		t.Fatal("容量错误响应格式不符", err)
	}
	if rejected.Error.Code != "TEMP_SPACE_EXHAUSTED" || rejected.Error.Message != "上传临时空间不足，请稍后重试" ||
		rejected.Error.Details != nil || rejected.RequestID == "" || rejected.RequestID != response.Header.Get("X-Request-ID") {
		t.Fatal("容量错误响应与公开契约不符", rejected)
	}
}
