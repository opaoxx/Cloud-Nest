package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

const serverCreateUploadBody = `{"parent_id":"33","name":"正式上传 🌱.bin","size_bytes":40}`
const serverCreateUploadKey = "server-upload-create-007"

// requestServerCreateUpload 经真实 run 入口调用正式上传创建接口。
func requestServerCreateUpload(t *testing.T, baseURL, token, key, body string, status int, code, field string) dto.CreateUploadResponse {
	t.Helper()
	requestID := uuid.NewString()
	message, err := http.NewRequest("POST", baseURL+"/api/v1/uploads?user_id=1&note=private-upload-create-query", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	message.Header.Set("Content-Type", "application/json")
	message.Header.Set("Origin", "http://127.0.0.1:5173")
	message.Header.Set("X-Request-ID", strings.ToUpper(requestID))
	message.Header.Set("X-User-ID", "1")
	message.AddCookie(&http.Cookie{Name: "cloudnest_refresh_token", Value: "private-upload-create-cookie"})
	if token != "" {
		message.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		message.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(message)
	if err != nil {
		t.Fatal("正式上传创建请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status || response.ContentLength != int64(len(encoded)) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || len(response.Cookies()) != 0 {
		t.Fatal("正式上传创建状态、响应头或正文错误", response.StatusCode, readError, closeError)
	}
	if strings.Contains(string(encoded), "private-upload-create") || strings.Contains(string(encoded), "user_id") {
		t.Fatal("正式上传创建响应包含原始请求或内部数据")
	}
	if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
		t.Fatal("正式上传创建认证提示与状态不符")
	}
	var payload dto.CreateUploadResponse
	if status == 201 {
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式上传创建成功 JSON 错误")
		}
		stable, err := json.Marshal(payload.Data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dto.RestoreCreateUploadData(stable); err != nil {
			t.Fatal("正式上传创建快照校验失败", err)
		}
		return payload
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != requestID || failure.Error.Message == "" {
		t.Fatal("正式上传创建错误结构不符")
	}
	if code == "FILE_TOO_LARGE" {
		if len(failure.Error.Details) != 2 || failure.Error.Details["field"] != "size_bytes" || failure.Error.Details["max_size_bytes"] != float64(domain.MaxSingleUploadSizeBytes) {
			t.Fatal("正式文件上限详情错误")
		}
	} else if field != "" {
		if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
			t.Fatal("正式上传创建字段定位错误")
		}
	} else if failure.Error.Details != nil {
		t.Fatal("普通上传创建错误 details 应为 null")
	}
	return payload
}

// serverUploadCreationState 保存专用测试账号 3 的上传创建事实。
type serverUploadCreationState struct{ reserved, taskReserved, objects, tasks, records uint64 }

func readServerUploadCreationState(t *testing.T, ctx context.Context, pool *sql.DB) serverUploadCreationState {
	t.Helper()
	var state serverUploadCreationState
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,(SELECT COALESCE(SUM(reserved_bytes),0) FROM upload_tasks WHERE user_id=3),(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=3),(SELECT COUNT(*) FROM upload_tasks WHERE user_id=3),(SELECT COUNT(*) FROM idempotency_records WHERE user_id=3) FROM users WHERE id=3").Scan(&state.reserved, &state.taskReserved, &state.objects, &state.tasks, &state.records); err != nil {
		t.Fatal(err)
	}
	return state
}

// verifyServerUploadCreation 使用已有账号 3 核对创建与查询，保存快照供重启对照。
func verifyServerUploadCreation(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, otherUserToken string) (dto.CreateUploadResponse, serverUploadCreationState) {
	t.Helper()
	before := readServerUploadCreationState(t, ctx, pool)
	requestServerCreateUpload(t, baseURL, "", "", `{"private-broken":`, 401, "AUTH_REQUIRED", "")
	requestServerCreateUpload(t, baseURL, token, "", `{"private-broken":`, 400, "INVALID_ARGUMENT", "Idempotency-Key")
	requestServerCreateUpload(t, baseURL, token, "upload-invalid-name", `{"parent_id":"33","name":"CON.txt","size_bytes":0}`, 400, "INVALID_NAME", "name")
	requestServerCreateUpload(t, baseURL, token, "upload-foreign-parent", `{"parent_id":"11","name":"scope.bin","size_bytes":0}`, 404, "ENTRY_NOT_FOUND", "")
	requestServerCreateUpload(t, baseURL, otherUserToken, "upload-file-parent", `{"parent_id":"42","name":"scope.bin","size_bytes":0}`, 400, "INVALID_ARGUMENT", "parent_id")
	requestServerCreateUpload(t, baseURL, token, "upload-oversize", `{"parent_id":"33","name":"large.bin","size_bytes":16777217}`, 413, "FILE_TOO_LARGE", "size_bytes")
	requestServerCreateUpload(t, baseURL, token, "upload-invalid-sha", `{"parent_id":"33","name":"hash.bin","size_bytes":0,"sha256":"private-invalid-sha"}`, 400, "INVALID_ARGUMENT", "sha256")
	if readServerUploadCreationState(t, ctx, pool) != before {
		t.Fatal("正式上传拒绝请求产生了写入")
	}

	first := requestServerCreateUpload(t, baseURL, token, serverCreateUploadKey, serverCreateUploadBody, 201, "", "")
	afterFirst := readServerUploadCreationState(t, ctx, pool)
	if afterFirst.reserved != before.reserved+40 || afterFirst.taskReserved != afterFirst.reserved || afterFirst.objects != before.objects+1 || afterFirst.tasks != before.tasks+1 || afterFirst.records != before.records+1 {
		t.Fatal("正式创建四项事实不一致")
	}
	retry := requestServerCreateUpload(t, baseURL, token, serverCreateUploadKey, `{"size_bytes":40,"parent_id":"33","name":"  正式上传 🌱.bin  ","sha256":null}`, 201, "", "")
	if !reflect.DeepEqual(first.Data, retry.Data) || first.RequestID == retry.RequestID || readServerUploadCreationState(t, ctx, pool) != afterFirst {
		t.Fatal("正式重试未复用原任务和预占")
	}
	requestServerCreateUpload(t, baseURL, token, serverCreateUploadKey, `{"parent_id":"33","name":"changed.bin","size_bytes":40}`, 409, "IDEMPOTENCY_CONFLICT", "")
	requestServerCreateUpload(t, baseURL, otherUserToken, serverCreateUploadKey, serverCreateUploadBody, 404, "ENTRY_NOT_FOUND", "")
	zero := requestServerCreateUpload(t, baseURL, token, "server-upload-zero", `{"parent_id":"33","name":"empty.bin","size_bytes":0}`, 201, "", "")
	digest := requestServerCreateUpload(t, baseURL, token, "server-upload-digest", `{"parent_id":"33","name":"digest.bin","size_bytes":20,"sha256":"`+strings.Repeat("aB", 32)+`"}`, 201, "", "")
	if zero.Data.Task.SizeBytes != 0 || digest.Data.Task.SizeBytes != 20 {
		t.Fatal("正式零字节或摘要声明错误")
	}
	var expectedSHA []byte
	if err := pool.QueryRowContext(ctx, "SELECT expected_sha256 FROM upload_tasks WHERE user_id=3 AND id=?", digest.Data.Task.ID).Scan(&expectedSHA); err != nil || len(expectedSHA) != 32 {
		t.Fatal("正式摘要声明未按 32 字节保存", err)
	}
	for _, value := range expectedSHA {
		if value != 0xab {
			t.Fatal("正式摘要大小写转换错误")
		}
	}
	current := readServerUploadCreationState(t, ctx, pool)
	requestServerCreateUpload(t, baseURL, token, "server-upload-quota", `{"parent_id":"33","name":"quota.bin","size_bytes":41}`, 507, "QUOTA_EXCEEDED", "")
	if readServerUploadCreationState(t, ctx, pool) != current || current.reserved != 60 || current.reserved != current.taskReserved {
		t.Fatal("正式配额拒绝或预占汇总错误")
	}

	var ownerID, deviceID uint64
	var provider, bucket, key string
	if err := pool.QueryRowContext(ctx, "SELECT u.user_id,u.device_id,o.provider,o.bucket,o.object_key FROM upload_tasks u JOIN stored_objects o ON o.id=u.staged_object_id WHERE u.id=?", first.Data.Task.ID).Scan(&ownerID, &deviceID, &provider, &bucket, &key); err != nil {
		t.Fatal(err)
	}
	storageConfig, err := config.LoadStorageConfig()
	if err != nil || ownerID != 3 || deviceID != 23 || provider != storageConfig.Provider || bucket != storageConfig.Bucket || !strings.HasPrefix(key, "users/3/objects/") {
		t.Fatal("生产存储配置或当前身份未准确装配", err)
	}
	var recordBody []byte
	var resourceID uint64
	var createdAt, expiresAt time.Time
	if err := pool.QueryRowContext(ctx, "SELECT resource_id,response_body,created_at,expires_at FROM idempotency_records WHERE user_id=3 AND operation=? AND idempotency_key=?", domain.CreateUploadOperation, serverCreateUploadKey).Scan(&resourceID, &recordBody, &createdAt, &expiresAt); err != nil {
		t.Fatal(err)
	}
	saved, err := dto.RestoreCreateUploadData(recordBody)
	if err != nil || !reflect.DeepEqual(saved, &first.Data) || strconv.FormatUint(resourceID, 10) != first.Data.Task.ID || expiresAt.Sub(createdAt) < 7*24*time.Hour {
		t.Fatal("正式保存结果的关联、快照或保留期错误", err)
	}
	queried := requestServerUploadTask(t, baseURL, token, first.Data.Task.ID, 200, "", "")
	if !reflect.DeepEqual(queried.Data, first.Data.Task) {
		t.Fatal("正式创建与查询的公开任务事实不一致")
	}
	requestServerUploadTask(t, baseURL, otherUserToken, first.Data.Task.ID, 404, "UPLOAD_NOT_FOUND", "")
	if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='VERIFYING',expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE user_id=3 AND id=?", first.Data.Task.ID); err != nil {
		t.Fatal(err)
	}
	currentTask := requestServerUploadTask(t, baseURL, token, first.Data.Task.ID, 200, "", "")
	if currentTask.Data.Status != "VERIFYING" || !currentTask.Data.ExpiresAt.Before(time.Now()) {
		t.Fatal("当前任务状态与期限变更未生效")
	}
	return first, current
}

// verifyServerUploadCreationAfterRestart 对照重启前快照及配额，验证原键继续复用。
func verifyServerUploadCreationAfterRestart(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token string, before dto.CreateUploadResponse, state serverUploadCreationState) {
	t.Helper()
	replayed := requestServerCreateUpload(t, baseURL, token, serverCreateUploadKey, serverCreateUploadBody, 201, "", "")
	if !reflect.DeepEqual(before.Data, replayed.Data) || replayed.RequestID == before.RequestID || readServerUploadCreationState(t, ctx, pool) != state {
		t.Fatal("真实重启后未复用原始上传创建结果")
	}
	current := requestServerUploadTask(t, baseURL, token, before.Data.Task.ID, 200, "", "")
	if current.Data.Status != "VERIFYING" || !current.Data.ExpiresAt.Before(time.Now()) {
		t.Fatal("重启后当前任务事实不准确")
	}
	requestServerCreateUpload(t, baseURL, token, serverCreateUploadKey, `{"parent_id":"33","name":"changed-after-restart.bin","size_bytes":40}`, 409, "IDEMPOTENCY_CONFLICT", "")
	if readServerUploadCreationState(t, ctx, pool) != state {
		t.Fatal("重启后冲突请求产生了额外事实")
	}
}
