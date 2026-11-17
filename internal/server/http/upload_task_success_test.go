package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func uploadTaskSuccessFixture() *domain.UploadTask {
	stamp := time.Date(2026, 11, 17, 10, 30, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	diagnostic, leaseOwner := "private-upload-diagnostic", "private-upload-worker"
	leaseUntil := stamp.Add(time.Minute)
	return &domain.UploadTask{
		ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13,
		RequestedName: "学习笔记.MD", UploadMode: "SINGLE", ExpectedSHA256: new([32]byte),
		FileSize: 2048, Status: domain.UploadStatusUploading, ReservedBytes: 2048,
		LeaseOwner: &leaseOwner, LeaseUntil: &leaseUntil, LeaseVersion: 3, AttemptCount: 2,
		LastErrorMessage: &diagnostic, CreatedAt: stamp, UpdatedAt: stamp.Add(time.Second), ExpiresAt: stamp.Add(24 * time.Hour),
	}
}

func TestUploadTaskSuccessDelivery(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	modes := []string{"existing id", "uppercase id", "generated id", "invalid id", "duplicate id", "allowed origin", "pending body headers", "zero bytes", "maximum identifiers", "safe byte boundary", "known error", "unknown error"}
	for _, status := range []string{"CREATING", "UPLOADING", "COMPLETING", "VERIFYING", "COMPLETED", "CANCELLING", "CANCELLED", "EXPIRING", "EXPIRED", "FAILED"} {
		modes = append(modes, status)
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备任务及请求 ID 场景，以公共中间件交付响应。
			result := uploadTaskSuccessFixture()
			requestIDs, expectedID := []string{requestID}, requestID
			switch mode {
			case "uppercase id":
				requestIDs = []string{strings.ToUpper(requestID)}
			case "generated id":
				requestIDs, expectedID = nil, ""
			case "invalid id":
				requestIDs, expectedID = []string{"private-request-id"}, ""
			case "duplicate id":
				requestIDs, expectedID = []string{requestID, requestID}, ""
			case "zero bytes":
				result.FileSize, result.ReservedBytes = 0, 0
			case "maximum identifiers":
				maximum := ^uint64(0)
				result.ID, result.UserID, result.TargetParentID, result.ResultEntryID = maximum, maximum, maximum, &maximum
			case "safe byte boundary":
				result.FileSize = 9007199254740991
			case "known error":
				code := "CHECKSUM_MISMATCH"
				result.LastErrorCode = &code
			case "unknown error":
				code := "private-upload-code"
				result.LastErrorCode = &code
			default:
				if domain.UploadStatus(mode).IsValid() {
					result.Status = domain.UploadStatus(mode)
					if result.Status.IsTerminal() {
						result.ReservedBytes = 0
					}
					if result.Status == domain.UploadStatusCompleted {
						name, entryID, stamp := "发布笔记.MD", uint64(31), result.UpdatedAt
						result.ResolvedName, result.ResultEntryID, result.CompletedAt = &name, &entryID, &stamp
					}
				}
			}
			expected, err := dto.NewUploadTaskData(result)
			if err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			var storedID string
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.GET("/test-upload-task-success", func(requestContext *gin.Context) {
				storedID = requestcontext.RequestID(requestContext.Request.Context())
				if mode == "pending body headers" {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.UploadTaskSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("任务成功交付后进入下游") })
			request := httptest.NewRequest("GET", "/test-upload-task-success?note=private-query", nil)
			for _, value := range requestIDs {
				request.Header.Add("X-Request-ID", value)
			}
			if mode == "allowed origin" {
				request.Header.Set("Origin", origin)
			}
			request.Header.Set("Authorization", "Bearer private-access")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对公开正文、缓存/正文头和三处请求 ID。
			if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" ||
				response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("任务成功状态或响应头错误")
			}
			var body dto.UploadTaskResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *expected) || body.RequestID == "" ||
				body.RequestID != storedID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("任务 JSON 或请求关联错误")
			}
			parsedID, err := uuid.Parse(body.RequestID)
			if err != nil || parsedID.Version() != 4 || (expectedID != "" && body.RequestID != expectedID) {
				t.Fatal("任务请求 ID 生成或规范化错误")
			}
			var envelope map[string]json.RawMessage
			var fields map[string]json.RawMessage
			if json.Unmarshal(response.Body.Bytes(), &envelope) != nil || len(envelope) != 2 || json.Unmarshal(envelope["data"], &fields) != nil || len(fields) != 13 {
				t.Fatal("任务响应字段数量错误")
			}
			if mode == "allowed origin" && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("任务成功响应缺少 CORS 头")
			}

			// 3. 核对完成日志、内部信息保护和来源原值。
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(200) || completed["method"] != "GET" || completed["path"] != "/test-upload-task-success" {
				t.Fatal("任务完成日志关联错误")
			}
			for _, marker := range []string{"private-query", "private-access", "private-refresh", "private-request-id", "private-upload"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("任务响应或日志包含内部原文")
				}
			}
			if strings.Contains(logs.String(), result.RequestedName) {
				t.Fatal("任务完成日志包含任务正文")
			}
			after, err := json.Marshal(result)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("任务成功交付修改了来源")
			}
		})
	}
}

func TestUploadTaskSuccessFailures(t *testing.T) {
	for _, mode := range []string{"nil result", "zero id", "zero user", "zero parent", "unknown status", "unknown mode", "invalid name", "unsafe size", "missing completed result", "missing completed time", "zero time", "late time", "cancelled", "deadline exceeded", "cancel before write"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 覆盖 DTO 转换失败、预先取消及预编码后取消。
			result := uploadTaskSuccessFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch mode {
			case "nil result":
				result = nil
			case "zero id":
				result.ID = 0
			case "zero user":
				result.UserID = 0
			case "zero parent":
				result.TargetParentID = 0
			case "unknown status":
				result.Status = "UNKNOWN"
			case "unknown mode":
				result.UploadMode = "UNKNOWN"
			case "invalid name":
				result.RequestedName = "\xff"
			case "unsafe size":
				result.FileSize = 9007199254740992
			case "missing completed result":
				stamp := result.UpdatedAt
				result.Status, result.CompletedAt = domain.UploadStatusCompleted, &stamp
			case "missing completed time":
				entryID := uint64(31)
				result.Status, result.ResultEntryID = domain.UploadStatusCompleted, &entryID
			case "zero time":
				result.CreatedAt = time.Time{}
			case "late time":
				result.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "cancelled":
				cancel()
			case "deadline exceeded":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				callContext = expired
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			router.GET("/test-upload-task-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				if mode == "cancel before write" {
					lateContext, stop := context.WithCancel(requestContext.Request.Context())
					defer stop()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: stop})
				}
				handler.UploadTaskSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("任务失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-upload-task-success?note=private-query", nil).WithContext(callContext))
			// 2. 统一错误整体交付，清理待发送正文头并关联请求 ID。
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Message != "服务内部错误，请稍后重试" || body.Error.Details != nil ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" {
				t.Fatal("任务失败交付结构或响应头错误")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["status"] != float64(500) || completed["level"] != "ERROR" || completed["request_id"] != body.RequestID {
				t.Fatal("任务失败完成日志错误")
			}
			if strings.Contains(response.Body.String(), `"data"`) || strings.Contains(response.Body.String(), "private-upload") || strings.Contains(logs.String(), "private-query") {
				t.Fatal("任务失败交付包含部分结果或内部原文")
			}
		})
	}
}

func TestUploadTaskSuccessCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body written", "headers flushed"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 已提交的响应通过 Written 检查直接结束本次处理。
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-upload-task-success", func(requestContext *gin.Context) {
				if mode == "body written" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
				cancelled, stop := context.WithCancel(requestContext.Request.Context())
				stop()
				requestContext.Request = requestContext.Request.WithContext(cancelled)
				handler.UploadTaskSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已提交任务响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-upload-task-success", nil))
			expectedBody, expectedType := "", ""
			if mode == "body written" {
				expectedBody, expectedType = "already-written", "text/plain; charset=utf-8"
			}
			if response.Code != 202 || response.Body.String() != expectedBody || response.Header().Get("Content-Type") != expectedType || response.Header().Get("Cache-Control") != "" || response.Header().Get("Pragma") != "" {
				t.Fatal("已提交任务响应的状态、正文或头发生变化")
			}
		})
	}
}

func TestUploadTaskSuccessHTTP(t *testing.T) {
	for _, mode := range []string{"uploading", "completed", "public error", "invalid data"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 使用系统分配端口，验证两次真实 GET 的正文和传输头。
			result, expectedStatus := uploadTaskSuccessFixture(), 200
			switch mode {
			case "completed":
				entryID, stamp := uint64(31), result.UpdatedAt
				result.Status, result.ReservedBytes, result.ResultEntryID, result.CompletedAt = domain.UploadStatusCompleted, 0, &entryID, &stamp
			case "public error":
				code := "CHECKSUM_MISMATCH"
				result.LastErrorCode = &code
			case "invalid data":
				result, expectedStatus = nil, 500
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-upload-task-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				handler.UploadTaskSuccess(requestContext, result)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			var previousID string
			var previousData dto.UploadTaskData
			for _, requestID := range []string{"28EE758A-0727-42DE-AE7D-3BB33155E148", "28EE758A-0727-42DE-AE7D-3BB33155E149"} {
				request, err := http.NewRequest("GET", server.URL+"/test-upload-task-success", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("X-Request-ID", requestID)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("任务真实 HTTP 请求失败", err)
				}
				encoded, readError := io.ReadAll(response.Body)
				closeError := response.Body.Close()
				if readError != nil || closeError != nil || response.StatusCode != expectedStatus || response.ContentLength != int64(len(encoded)) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
					response.Header.Get("Content-Encoding") != "" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || len(response.Cookies()) != 0 {
					t.Fatal("任务真实响应状态、正文或头错误", readError, closeError)
				}
				currentID := response.Header.Get("X-Request-ID")
				if currentID != strings.ToLower(requestID) || currentID == previousID {
					t.Fatal("任务真实响应请求 ID 错误")
				}
				if expectedStatus == 200 {
					var body dto.UploadTaskResponse
					expected, err := dto.NewUploadTaskData(result)
					if err != nil || json.Unmarshal(encoded, &body) != nil || body.RequestID != currentID || !reflect.DeepEqual(body.Data, *expected) ||
						(previousID != "" && !reflect.DeepEqual(previousData, body.Data)) {
						t.Fatal("任务真实成功 JSON 或重复交付错误", err)
					}
					previousData = body.Data
				} else {
					var body dto.ErrorResponse
					if json.Unmarshal(encoded, &body) != nil || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil || body.RequestID != currentID {
						t.Fatal("任务真实统一错误 JSON 错误")
					}
				}
				previousID = currentID
			}
		})
	}
}
