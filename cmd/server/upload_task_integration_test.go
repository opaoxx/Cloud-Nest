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
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// requestServerUploadTask 经真实启动入口查询任务，核对公开正文与传输元数据。
func requestServerUploadTask(t *testing.T, baseURL, token, path string, status int, code, field string) dto.UploadTaskResponse {
	t.Helper()
	request, err := http.NewRequest("GET", baseURL+"/api/v1/uploads/"+path+"?id=99&user_id=3&note=private-upload-query", strings.NewReader(`{"id":"99","user_id":"3"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	request.Header.Set("X-User-ID", "3")
	request.AddCookie(&http.Cookie{Name: "cloudnest_refresh_token", Value: "private-upload-cookie"})
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("正式上传任务查询请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	requestID := response.Header.Get("X-Request-ID")
	parsedID, idError := uuid.Parse(requestID)
	if readError != nil || closeError != nil || response.StatusCode != status || response.ContentLength != int64(len(encoded)) ||
		response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" ||
		response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" ||
		len(response.Cookies()) != 0 || idError != nil || parsedID.Version() != 4 {
		t.Fatal("正式上传任务状态、正文或响应头错误", response.StatusCode, readError, closeError)
	}
	if strings.Contains(string(encoded), "private-upload") || strings.Contains(string(encoded), "user_id") {
		t.Fatal("正式上传任务响应包含请求或内部信息")
	}
	if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
		t.Fatal("正式上传任务认证提示与状态不符")
	}
	var payload dto.UploadTaskResponse
	if status == 200 {
		var outer map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" ||
			json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &fields) != nil || len(fields) != 13 {
			t.Fatal("正式上传任务公开 JSON 错误")
		}
		return payload
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.RequestID != requestID || failure.Error.Code != code || failure.Error.Message == "" {
		t.Fatal("正式上传任务错误结构错误")
	}
	if field != "" {
		if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
			t.Fatal("正式上传任务错误字段不符")
		}
	} else if failure.Error.Details != nil {
		t.Fatal("正式上传任务普通错误包含额外详情")
	}
	return payload
}

// verifyServerUploadTasks 建立专用任务事实，保存两类结果供真实服务重启后对照。
func verifyServerUploadTasks(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, otherDeviceToken, foreignToken string) (dto.UploadTaskResponse, dto.UploadTaskResponse) {
	t.Helper()
	// 1. 十种阶段使用独立暂存对象，完成任务保留已有逻辑文件的审计引用。
	stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
	// 用活跃租约保留这些查询夹具，过期处理由独立的上传创建用例验证。
	var fixtureLeaseUntil time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)+INTERVAL 1 HOUR").Scan(&fixtureLeaseUntil); err != nil {
		t.Fatal(err)
	}
	var uploading, completed dto.UploadTaskResponse
	for index, status := range []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting,
		domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling,
		domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed,
	} {
		taskID := uint64(500001 + index)
		size := uint64(0)
		var publishedID, resultID any
		var resolvedName, completedAt any
		var leaseOwner, leaseUntil any
		var leaseVersion uint64
		if !status.IsTerminal() {
			leaseOwner, leaseUntil, leaseVersion = "upload-query-fixture", fixtureLeaseUntil, 1
		}
		if status == domain.UploadStatusCompleted {
			size, publishedID, resultID, resolvedName, completedAt = 2048, uint64(41), uint64(23), "upload-route-final.bin", stamp
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,1,'MINIO','fixture-bucket',?,?)", taskID, "upload-route-object-"+strconv.FormatUint(taskID, 10), size); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,published_object_id,target_parent_id,requested_name,resolved_name,
			upload_mode,file_size,status,result_entry_id,reserved_bytes,expires_at,last_error_code,last_error_message,created_at,updated_at,completed_at,
			lease_owner,lease_until,lease_version) VALUES(?,1,13,?,?,11,?,?,'SINGLE',?,?,?,0,?,'STORAGE_RETRY','private-upload-diagnostic',?,?,?,?,?,?)`,
			taskID, taskID, publishedID, "upload-route-"+strconv.FormatUint(taskID, 10)+".bin", resolvedName, size, string(status), resultID,
			stamp.Add(-24*time.Hour), stamp, stamp, completedAt, leaseOwner, leaseUntil, leaseVersion); err != nil {
			t.Fatal(err)
		}
		response := requestServerUploadTask(t, baseURL, token, strconv.FormatUint(taskID, 10), 200, "", "")
		if response.Data.ID != strconv.FormatUint(taskID, 10) || response.Data.ParentID != "11" || response.Data.Status != string(status) || response.Data.UploadMode != "SINGLE" ||
			response.Data.SizeBytes != size || response.Data.LastError == nil || response.Data.LastError.Code != "INTERNAL_ERROR" || !response.Data.ExpiresAt.Equal(stamp.Add(-24*time.Hour)) {
			t.Fatal("正式上传任务阶段或字段映射错误")
		}
		if status == domain.UploadStatusCompleted {
			if response.Data.ResultEntryID == nil || *response.Data.ResultEntryID != "23" || response.Data.ResolvedName == nil || *response.Data.ResolvedName != "upload-route-final.bin" || response.Data.CompletedAt == nil {
				t.Fatal("正式完成任务结果错误")
			}
			completed = response
		} else if response.Data.ResultEntryID != nil || response.Data.ResolvedName != nil || response.Data.CompletedAt != nil {
			t.Fatal("正式未发布任务结果空值错误")
		}
		if status == domain.UploadStatusUploading {
			uploading = response
		}
	}
	// 2. 正式路径解析、认证前置和跨账号任务缺失保持固定响应。
	requestServerUploadTask(t, baseURL, "", "0", 401, "AUTH_REQUIRED", "")
	requestServerUploadTask(t, baseURL, token, "0", 400, "INVALID_ARGUMENT", "id")
	requestServerUploadTask(t, baseURL, token, "0500002", 400, "INVALID_ARGUMENT", "id")
	requestServerUploadTask(t, baseURL, token, "18446744073709551616", 400, "INVALID_ARGUMENT", "id")
	requestServerUploadTask(t, baseURL, token, "%2535%2530%2530%2530%2530%2532", 400, "INVALID_ARGUMENT", "id")
	encoded := requestServerUploadTask(t, baseURL, token, "%35%30%30%30%30%32", 200, "", "")
	otherDevice := requestServerUploadTask(t, baseURL, otherDeviceToken, "500002", 200, "", "")
	if encoded.Data.ID != uploading.Data.ID || otherDevice.Data.ID != uploading.Data.ID {
		t.Fatal("正式路径编码或同用户跨设备查询错误")
	}
	requestServerUploadTask(t, baseURL, token, "599999", 404, "UPLOAD_NOT_FOUND", "")
	requestServerUploadTask(t, baseURL, foreignToken, "500002", 404, "UPLOAD_NOT_FOUND", "")
	if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(500011,3,'MINIO','fixture-bucket','upload-route-foreign',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(500011,3,23,500011,33,'upload-route-foreign.bin','SINGLE',0,'UPLOADING',0,?)", fixtureLeaseUntil); err != nil {
		t.Fatal(err)
	}
	requestServerUploadTask(t, baseURL, token, "500011", 404, "UPLOAD_NOT_FOUND", "")
	if foreign := requestServerUploadTask(t, baseURL, foreignToken, "500011", 200, "", ""); foreign.Data.ParentID != "33" {
		t.Fatal("正式其他用户的独立任务查询错误")
	}
	// 3. 通过同一正式服务确认不同阶段，并核对授权和归属。
	requestServerUploadCompletion(t, ctx, baseURL, "", "0", 401, "AUTH_REQUIRED", "")
	requestServerUploadCompletion(t, ctx, baseURL, token, "0", 400, "INVALID_ARGUMENT", "id")
	requestServerUploadCompletion(t, ctx, baseURL, foreignToken, completed.Data.ID, 404, "UPLOAD_NOT_FOUND", "")
	requestServerUploadCompletion(t, ctx, baseURL, token, "599999", 404, "UPLOAD_NOT_FOUND", "")
	requestServerUploadCompletion(t, ctx, baseURL, token, uploading.Data.ID, 409, "TASK_NOT_READY", "")
	verifying := requestServerUploadCompletion(t, ctx, baseURL, otherDeviceToken, "500004", 202, "", "")
	if verifying.Data.Status != "VERIFYING" || verifying.Data.ResultEntryID != nil || verifying.Data.CompletedAt != nil ||
		verifying.Data.LastError == nil || verifying.Data.LastError.Code != "INTERNAL_ERROR" {
		t.Fatal("跨设备确认未返回持久化的校验中快照", verifying.Data)
	}
	confirmed := requestServerUploadCompletion(t, ctx, baseURL, token, completed.Data.ID, 200, "", "")
	if !reflect.DeepEqual(confirmed.Data, completed.Data) || confirmed.RequestID == completed.RequestID {
		t.Fatal("正式完成确认与已有发布结果不符", confirmed.Data)
	}
	return uploading, completed
}

// requestServerUploadCompletion 通过正式 POST 核对完成确认的公开响应。
func requestServerUploadCompletion(t *testing.T, ctx context.Context, baseURL, token, taskID string, status int, code, field string) dto.UploadTaskResponse {
	t.Helper()
	// 1. 发送无正文请求，凭证通过 Authorization 传入。
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/uploads/"+taskID+"/complete", nil)
	if err != nil {
		t.Fatal(err)
	}
	const origin = "http://127.0.0.1:5173"
	request.Header.Set("Origin", origin)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("正式上传完成确认请求失败", err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	requestID := response.Header.Get("X-Request-ID")
	if err != nil || response.StatusCode != status || requestID == "" || response.Header.Get("Cache-Control") != "no-store" ||
		response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatal("正式上传完成确认状态、正文或响应头不符", response.StatusCode, status, err, string(encoded))
	}
	// 2. 校验公开任务或固定错误，并核对请求 ID。
	var payload dto.UploadTaskResponse
	if code == "" {
		if json.Unmarshal(encoded, &payload) != nil || payload.Data.ID != taskID || payload.RequestID != requestID {
			t.Fatal("正式上传完成确认成功响应不符", string(encoded))
		}
		return payload
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.RequestID != requestID || failure.Error.Code != code ||
		(field == "" && failure.Error.Details != nil) || (field != "" && (len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field)) {
		t.Fatal("正式上传完成确认错误结构不符", string(encoded))
	}
	return payload
}
