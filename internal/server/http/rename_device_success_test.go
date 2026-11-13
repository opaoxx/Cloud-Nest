package http

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
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func renameDeviceSuccessFixture() *domain.UserDevice {
	return &domain.UserDevice{
		ID: 13, UserID: 7, Name: "  学习电脑 🎮  ", Platform: "WEB",
		CreatedAt: time.Date(2026, 11, 13, 8, 0, 0, 123456789, time.FixedZone("UTC+8", 8*60*60)),
	}
}

func TestRenameDeviceSuccessDelivery(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, testCase := range []struct {
		name                              string
		requestIDs                        []string
		expectedID                        string
		origin, pending, maximum, revoked bool
	}{
		{name: "existing request id", requestIDs: []string{requestID}, expectedID: requestID},
		{name: "uppercase request id", requestIDs: []string{strings.ToUpper(requestID)}, expectedID: requestID},
		{name: "generated request id"},
		{name: "invalid request id", requestIDs: []string{"private-request-id"}},
		{name: "duplicate request id", requestIDs: []string{requestID, requestID}},
		{name: "allowed origin", requestIDs: []string{requestID}, expectedID: requestID, origin: true},
		{name: "full unsigned device id", maximum: true},
		{name: "revoked device", revoked: true},
		{name: "pending body headers", requestIDs: []string{requestID}, expectedID: requestID, pending: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试入口只交付已经确认的结果，公共中间件提供请求 ID。
			result := renameDeviceSuccessFixture()
			if testCase.maximum {
				result.ID = ^uint64(0)
			}
			if testCase.revoked {
				revokedAt := result.CreatedAt.Add(time.Minute)
				result.LastSeenAt, result.RevokedAt = &revokedAt, &revokedAt
			}
			var logs bytes.Buffer
			var storedID string
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.PATCH("/test-rename-device-success", func(requestContext *gin.Context) {
				storedID = requestcontext.RequestID(requestContext.Request.Context())
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.RenameDeviceSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("成功交付后进入下游") })
			request := httptest.NewRequest("PATCH", "/test-rename-device-success?note=private-query", nil)
			for _, value := range testCase.requestIDs {
				request.Header.Add("X-Request-ID", value)
			}
			if testCase.origin {
				request.Header.Set("Origin", origin)
			}
			request.Header.Set("Authorization", "Bearer private-access")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 检查正文、缓存及正文相关头，关联上下文、响应和日志中的 ID。
			if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) ||
				response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("成功状态或响应头错误")
			}
			var body dto.RenameDeviceResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Data.Device.Name != "  学习电脑 🎮  " || body.RequestID == "" ||
				body.RequestID != storedID || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("成功 JSON 或请求 ID 错误")
			}
			parsedID, err := uuid.Parse(body.RequestID)
			if err != nil || parsedID.Version() != 4 || (testCase.expectedID != "" && body.RequestID != testCase.expectedID) {
				t.Fatal("请求 ID 生成、保留或规范化错误")
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 2 {
				t.Fatal("成功响应包含非约定字段")
			}
			var payload map[string]json.RawMessage
			if json.Unmarshal(fields["data"], &payload) != nil || len(payload) != 1 {
				t.Fatal("设备响应 data 结构错误")
			}
			var device map[string]any
			if json.Unmarshal(payload["device"], &device) != nil || len(device) != 6 || device["id"] != strconv.FormatUint(result.ID, 10) || device["name"] != result.Name || device["platform"] != result.Platform || device["created_at"] != "2026-11-13T00:00:00.123456Z" {
				t.Fatal("设备公开字段、ID 或时间错误")
			}
			if (body.Data.Device.LastSeenAt != nil) != testCase.revoked || (body.Data.Device.RevokedAt != nil) != testCase.revoked {
				t.Fatal("可空时间状态错误")
			}
			if testCase.revoked {
				if !body.Data.Device.RevokedAt.Equal(result.RevokedAt.UTC().Truncate(time.Microsecond)) || device["last_seen_at"] != "2026-11-13T00:01:00.123456Z" || device["revoked_at"] != "2026-11-13T00:01:00.123456Z" {
					t.Fatal("历史设备时间事实错误")
				}
			} else if device["last_seen_at"] != nil || device["revoked_at"] != nil {
				t.Fatal("空时间未明确交付 null")
			}
			var completed map[string]any
			if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != body.RequestID || completed["status"] != float64(200) || completed["path"] != "/test-rename-device-success" {
				t.Fatal("完成日志的请求 ID、状态或路径错误")
			}
			if testCase.origin && response.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("成功响应缺少 CORS 头")
			}

			// 3. 检查原始请求值保护及输入结果。
			for _, marker := range []string{"private-query", "private-access", "private-refresh", "private-request-id"} {
				if strings.Contains(response.Body.String(), marker) || strings.Contains(logs.String(), marker) {
					t.Fatal("正文或完成日志包含原始请求值")
				}
			}
			if result.Name != "  学习电脑 🎮  " || result.CreatedAt.Location().String() != "UTC+8" || result.CreatedAt.Nanosecond() != 123456789 {
				t.Fatal("交付修改了服务结果")
			}
		})
	}
}

func TestRenameDeviceSuccessFailures(t *testing.T) {
	for _, failure := range []string{"nil result", "zero device", "zero owner", "invalid creation time", "invalid last seen", "invalid revoked time", "cancelled", "deadline exceeded", "cancel before write"} {
		t.Run(failure, func(t *testing.T) {
			result := renameDeviceSuccessFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch failure {
			case "nil result":
				result = nil
			case "zero device":
				result.ID = 0
			case "zero owner":
				result.UserID = 0
			case "invalid creation time":
				result.CreatedAt = time.Time{}
			case "invalid last seen":
				invalid := time.Time{}
				result.LastSeenAt = &invalid
			case "invalid revoked time":
				invalid := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				result.RevokedAt = &invalid
			case "cancelled":
				cancel()
			case "deadline exceeded":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				callContext = expired
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PATCH("/test-rename-device-success", func(requestContext *gin.Context) {
				requestContext.Header("Content-Length", "3")
				requestContext.Header("Content-Encoding", "gzip")
				requestContext.Header("Content-Type", "application/octet-stream")
				if failure == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					// 复用已有测试上下文，在第二次 Err 检查时取消。
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.RenameDeviceSuccess(requestContext, result)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("PATCH", "/test-rename-device-success", nil).WithContext(callContext))
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
				response.Header().Get("Content-Encoding") != "" || response.Header().Get("Content-Length") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("失败交付的错误结构或响应头错误")
			}
		})
	}
}

func TestRenameDeviceSuccessCommittedResponse(t *testing.T) {
	for _, name := range []string{"body written", "headers flushed"} {
		t.Run(name, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.PATCH("/test-rename-device-success", func(requestContext *gin.Context) {
				if name == "body written" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
				cancelled, cancel := context.WithCancel(requestContext.Request.Context())
				cancel()
				requestContext.Request = requestContext.Request.WithContext(cancelled)
				handler.RenameDeviceSuccess(requestContext, nil)
			}, func(*gin.Context) { t.Error("已提交响应后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("PATCH", "/test-rename-device-success", nil))
			expectedBody, expectedType := "", ""
			if name == "body written" {
				expectedBody, expectedType = "already-written", "text/plain; charset=utf-8"
			}
			if response.Code != 202 || response.Body.String() != expectedBody || response.Header().Get("Content-Type") != expectedType ||
				response.Header().Get("Cache-Control") != "" || response.Header().Get("Pragma") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("已提交响应被改写")
			}
		})
	}
}

func TestRenameDeviceSuccessHTTP(t *testing.T) {
	// 1. 使用空闲端口启动测试服务，验证实际 HTTP 传输。
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.PATCH("/test-rename-device-success", func(requestContext *gin.Context) {
		handler.RenameDeviceSuccess(requestContext, renameDeviceSuccessFixture())
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	request, err := http.NewRequest("PATCH", server.URL+"/test-rename-device-success", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("真实 HTTP 请求失败", err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal("读取真实响应失败", err)
	}

	// 2. 核对网络响应状态、JSON、长度和缓存头。
	var body dto.RenameDeviceResponse
	if json.Unmarshal(encoded, &body) != nil || response.StatusCode != 200 || body.Data.Device.Name != "  学习电脑 🎮  " || body.Data.Device.ID != "13" || body.Data.Device.LastSeenAt != nil || body.Data.Device.RevokedAt != nil || body.RequestID != "28ee758a-0727-42de-ae7d-3bb33155e148" ||
		body.RequestID != response.Header.Get("X-Request-ID") || response.ContentLength != int64(len(encoded)) ||
		response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" ||
		response.Header.Get("Pragma") != "no-cache" || len(response.Cookies()) != 0 {
		t.Fatal("真实 HTTP 成功交付错误")
	}
}
