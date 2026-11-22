package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

const quotaRaceUploadsPath = "/api/v1/uploads"

type quotaRaceUpload struct {
	mode, name, key string
	size            uint64
}

type quotaRaceUploadResult struct {
	upload    quotaRaceUpload
	status    int
	requestID string
	body      []byte
	err       error
}

func TestServerCreateUploadQuotaRaceIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_CREATE_UPLOAD_QUOTA_RACE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_CREATE_UPLOAD_QUOTA_RACE_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("双模式配额专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	api := fixture.api
	userID := backupFixtureID(t, fixture.login.User.ID)

	// 1. 调整本轮独占库的空账号，留出单项可用、两项不能同时满足的配额。
	const quota uint64 = 20 << 20
	const singleSize uint64 = 16 << 20
	updated, err := fixture.pool.ExecContext(ctx, `UPDATE users SET storage_quota=?
		WHERE id=? AND used_storage=0 AND reserved_storage=0`, quota, userID)
	if err != nil {
		t.Fatal("设置本轮合成账号配额失败", err)
	}
	if count, err := updated.RowsAffected(); err != nil || count != 1 {
		t.Fatal("配额夹具未精确命中一个空账号", count, err)
	}
	var before dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &before)
	if before.RequestID == "" || before.Data.User.ID != fixture.login.User.ID ||
		before.Data.QuotaBytes != quota || before.Data.UsedBytes != 0 || before.Data.ReservedBytes != 0 {
		t.Fatal("正式资料未返回本轮可竞争的配额", before.Data)
	}

	// 2. 用同一登录和不同幂等键并发请求正式创建，等待两个响应后再核对结果。
	uploads := []quotaRaceUpload{
		{mode: "SINGLE", name: "quota-single.bin", key: "quota-race-single", size: singleSize},
		{mode: "MULTIPART", name: "quota-multipart.bin", key: "quota-race-multipart", size: singleSize + 1},
	}
	ready := make(chan struct{}, len(uploads))
	start := make(chan struct{})
	responses := make(chan quotaRaceUploadResult, len(uploads))
	for _, upload := range uploads {
		go func() {
			ready <- struct{}{}
			<-start
			responses <- postQuotaRaceUpload(ctx, api, fixture.login.RootEntryID, upload)
		}()
	}
	for range uploads {
		<-ready
	}
	close(start)
	results := []quotaRaceUploadResult{<-responses, <-responses}
	var winner, loser quotaRaceUploadResult
	for _, result := range results {
		if result.err != nil {
			t.Fatal("正式并发创建请求失败", result.upload.mode, result.err)
		}
		switch result.status {
		case http.StatusCreated:
			winner = result
		case http.StatusInsufficientStorage:
			loser = result
		default:
			t.Fatalf("%s 请求返回意外结果，HTTP %d：%s", result.upload.mode, result.status, result.body)
		}
	}
	if winner.status != http.StatusCreated || loser.status != http.StatusInsufficientStorage {
		t.Fatalf("并发请求须有一个 201 及一个 507，实际为 %d 和 %d", results[0].status, results[1].status)
	}
	var rejected dto.ErrorResponse
	if err := json.Unmarshal(loser.body, &rejected); err != nil || rejected.RequestID == "" ||
		rejected.RequestID != loser.requestID || rejected.Error.Code != "QUOTA_EXCEEDED" || rejected.Error.Details != nil {
		t.Fatal("额度不足请求未交付约定的错误响应", rejected, err)
	}
	created := readQuotaRaceWinner(t, winner, fixture.login.RootEntryID)

	// 3. 用正式查询和隔离库确认仅获准请求留下任务、暂存对象、幂等快照和预占。
	taskPath := quotaRaceUploadsPath + "/" + created.ID
	active := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	if active.ID != created.ID || active.UploadMode != winner.upload.mode || active.SizeBytes != winner.upload.size ||
		!active.ExpiresAt.Equal(created.ExpiresAt) || active.ResultEntryID != nil {
		t.Fatal("唯一获准任务的实时状态不符", active)
	}
	if active.UploadMode == "MULTIPART" {
		readServerMultipartPartTarget(t, ctx, fixture, active)
	}
	var after dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &after)
	if after.RequestID == "" || after.Data.User.ID != before.Data.User.ID || after.Data.QuotaBytes != quota ||
		after.Data.UsedBytes != 0 || after.Data.ReservedBytes != winner.upload.size {
		t.Fatal("并发创建后的公开配额不符", after.Data)
	}

	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal("打开专用库仓储失败", err)
	}
	taskID := backupFixtureID(t, created.ID)
	task, err := store.FindUserUploadTask(ctx, userID, taskID)
	if err != nil || task == nil || task.ReservedBytes != winner.upload.size || task.StagedObjectID == 0 ||
		task.RequestedName != winner.upload.name || task.UploadMode != winner.upload.mode {
		t.Fatal("隔离库任务未保留获准请求及其完整预占", task, err)
	}
	object, err := store.FindUserStoredObject(ctx, userID, task.StagedObjectID)
	if err != nil || object == nil || object.Status != domain.StoredObjectStatusStaging || object.Bucket != fixture.bucket.name ||
		object.FileSize != winner.upload.size || object.StorageVersionID != nil {
		t.Fatal("唯一暂存对象与获准任务不符", object, err)
	}
	var tasks, objects, snapshots, entries uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=users.id),
		(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=users.id),
		(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id AND operation=?),
		(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE')
		FROM users WHERE id=?`, domain.CreateUploadOperation, userID).
		Scan(&tasks, &objects, &snapshots, &entries); err != nil || tasks != 1 || objects != 1 || snapshots != 1 || entries != 0 {
		t.Fatal("并发创建留下了意外的任务、对象、幂等快照或发布条目", err, tasks, objects, snapshots, entries)
	}
	var storedKey string
	var resourceID uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT idempotency_key,resource_id FROM idempotency_records
		WHERE user_id=? AND operation=?`, userID, domain.CreateUploadOperation).
		Scan(&storedKey, &resourceID); err != nil || storedKey != winner.upload.key || resourceID != taskID {
		t.Fatal("唯一创建幂等快照未绑定获准请求", err, storedKey, resourceID)
	}

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("同账号额度 %d B，%s 声明 %d B 返回 201，%s 声明 %d B 返回 507 QUOTA_EXCEEDED，任务、暂存对象和创建快照均为 1，已用 %d B、预占 %d B",
		quota, winner.upload.mode, winner.upload.size, loser.upload.mode, loser.upload.size, after.Data.UsedBytes, after.Data.ReservedBytes)
}

func postQuotaRaceUpload(ctx context.Context, api backupSourceClient, rootID string, upload quotaRaceUpload) quotaRaceUploadResult {
	result := quotaRaceUploadResult{upload: upload}
	// 两种声明都带格式有效的合成摘要。本用例仅提交创建 JSON。
	body := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":%q}`,
		rootID, upload.name, upload.size, strings.Repeat("a5", 32))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, api.baseURL+quotaRaceUploadsPath, strings.NewReader(body))
	if err != nil {
		result.err = err
		return result
	}
	request.Header.Set("Authorization", "Bearer "+api.accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", upload.key)
	response, err := api.httpClient.Do(request)
	if err != nil {
		result.err = err
		return result
	}
	defer response.Body.Close()
	result.status = response.StatusCode
	result.requestID = response.Header.Get("X-Request-ID")
	result.body, result.err = io.ReadAll(io.LimitReader(response.Body, 64<<10))
	return result
}

func readQuotaRaceWinner(t *testing.T, winner quotaRaceUploadResult, rootID string) dto.UploadTaskData {
	t.Helper()
	// 1. 按声明模式恢复对应的正式创建快照。
	var reply struct {
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	if err := json.Unmarshal(winner.body, &reply); err != nil || reply.RequestID == "" || reply.RequestID != winner.requestID {
		t.Fatal("获准请求的创建响应不符", err)
	}
	var task dto.UploadTaskData
	if winner.upload.mode == "SINGLE" {
		created, err := dto.RestoreCreateUploadData(reply.Data)
		if err != nil || created.ContentPath != quotaRaceUploadsPath+"/"+created.Task.ID+"/content" {
			t.Fatal("获准 SINGLE 请求未交付正式内容路径", err)
		}
		task = created.Task
	} else {
		created, err := dto.RestoreCreateMultipartUploadData(reply.Data)
		if err != nil || created.PartSizeBytes != 16<<20 || created.PartCount != 2 {
			t.Fatal("获准 MULTIPART 请求未交付正式两片计划", err)
		}
		task = created.Task
	}
	// 2. 核对公开任务逐项绑定到获准声明。
	if task.ID == "" || task.UploadMode != winner.upload.mode || task.ParentID != rootID ||
		task.RequestedName != winner.upload.name || task.SizeBytes != winner.upload.size {
		t.Fatal("获准任务与原声明不符", task)
	}
	return task
}
