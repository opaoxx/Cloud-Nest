package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadTaskSuccessIntegration(t *testing.T) {
	// 1. 绑定正式任务读取和服务，以固定可信 Actor 验证成功交付入口。
	pool, want := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	tasks := service.NewUploadTaskService(store.FindUserUploadTask)
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	activeTaskID := uint64(19)
	var logs bytes.Buffer
	const origin = "http://127.0.0.1:5173"
	router := serverhttp.NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
	router.GET("/test-upload-task-success", func(requestContext *gin.Context) {
		result, err := tasks.Get(requestContext.Request.Context(), actor, activeTaskID)
		if err != nil {
			handler.InternalError(requestContext, nil)
			return
		}
		requestContext.Header("Content-Length", "3")
		requestContext.Header("Content-Encoding", "gzip")
		handler.UploadTaskSuccess(requestContext, result)
	}, func(*gin.Context) { t.Error("真实任务交付后进入下游") })
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	sequence := 0
	request := func(t *testing.T, taskID uint64, expectedStatus int) dto.UploadTaskResponse {
		t.Helper()
		activeTaskID = taskID
		sequence++
		message, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/test-upload-task-success?note=private-upload-query", strings.NewReader("private-upload-body"))
		if err != nil {
			t.Fatal(err)
		}
		requestID := "28ee758a-0727-42de-ae7d-" + strconv.FormatInt(300000000000+int64(sequence), 10)
		message.Header.Set("X-Request-ID", strings.ToUpper(requestID))
		message.Header.Set("Origin", origin)
		message.Header.Set("Authorization", "Bearer private-upload-access")
		message.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-upload-cookie"})
		response, err := client.Do(message)
		if err != nil {
			t.Fatal("真实任务 GET 失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != expectedStatus || response.ContentLength != int64(len(encoded)) ||
			response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" ||
			response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" ||
			response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != origin || len(response.Cookies()) != 0 {
			t.Fatal("真实任务交付状态或响应头错误", readError, closeError)
		}
		if strings.Contains(string(encoded), "private-upload") || strings.Contains(string(encoded), "曾经重试") {
			t.Fatal("真实任务响应包含内部原文")
		}
		if expectedStatus == 500 {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != "INTERNAL_ERROR" || failure.Error.Details != nil || failure.RequestID != requestID {
				t.Fatal("真实任务失败交付错误")
			}
			return dto.UploadTaskResponse{}
		}
		var result dto.UploadTaskResponse
		if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID || result.Data.ID != strconv.FormatUint(taskID, 10) {
			t.Fatal("真实任务成功 JSON 或请求 ID 错误")
		}
		return result
	}

	// 2. 十种任务阶段与公开错误摘要经过真实 TCP 交付。
	for _, status := range []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting,
		domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling,
		domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status=? WHERE user_id=7 AND id=19", string(status)); err != nil {
				t.Fatal(err)
			}
			result := request(t, 19, 200)
			if result.Data.Status != string(status) || result.Data.ResultEntryID == nil || *result.Data.ResultEntryID != "31" ||
				result.Data.LastError == nil || result.Data.LastError.Code != "INTERNAL_ERROR" {
				t.Fatal("真实任务状态、结果或通用错误摘要错误")
			}
		})
	}
	t.Run("known error summary", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET last_error_code='CHECKSUM_MISMATCH',last_error_message='private-upload-diagnostic' WHERE user_id=7 AND id=19"); err != nil {
			t.Fatal(err)
		}
		result := request(t, 19, 200)
		if result.Data.LastError == nil || result.Data.LastError.Code != "CHECKSUM_MISMATCH" || result.Data.LastError.Message != "文件校验失败，请重新上传" {
			t.Fatal("真实任务固定错误摘要错误")
		}
	})
	t.Run("zero byte task with null results", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(25,7,'MINIO','fixture-bucket','success-zero',0)"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at,created_at,updated_at) VALUES(20,7,11,25,13,'empty.bin','SINGLE',0,'UPLOADING',0,?,?,?)", want.ExpiresAt, want.CreatedAt, want.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		result := request(t, 20, 200)
		if result.Data.SizeBytes != 0 || result.Data.ResolvedName != nil || result.Data.ResultEntryID != nil || result.Data.CompletedAt != nil || result.Data.LastError != nil {
			t.Fatal("真实零字节任务或可空结果错误")
		}
	})
	t.Run("unsafe JSON size", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET file_size=9007199254740992 WHERE user_id=7 AND id=19"); err != nil {
			t.Fatal(err)
		}
		request(t, 19, 500)
	})
	// 3. 每次 GET 对应一条完成日志，任务正文和原始请求各自保留交付边界。
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != sequence || sequence != 13 {
		t.Fatal("真实任务完成日志数量错误")
	}
	for _, line := range lines {
		var completed map[string]any
		if json.Unmarshal([]byte(line), &completed) != nil || completed["request_id"] == "" || completed["method"] != "GET" || completed["path"] != "/test-upload-task-success" {
			t.Fatal("真实任务完成日志结构错误")
		}
	}
	for _, marker := range []string{"private-upload", "学习笔记", "发布笔记", "曾经重试"} {
		if strings.Contains(logs.String(), marker) {
			t.Fatal("真实任务完成日志包含请求或任务原文")
		}
	}
}
