package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestUploadTaskHandlerFlow(t *testing.T) {
	for _, fixture := range []struct {
		name, path, code                                  string
		status, calls, cancelOn                           int
		taskID                                            uint64
		serviceError                                      error
		change                                            func(*domain.UploadTask)
		missingActor, nilResult, maximumActor             bool
		cancelBefore, cancelAfter, deadline, panicService bool
		pending, noOrigin, forbiddenOrigin                bool
	}{
		{name: "task", status: 200, calls: 1},
		{name: "decoded path", path: "%31%39", status: 200, calls: 1},
		{name: "no origin", noOrigin: true, status: 200, calls: 1},
		{name: "pending success", pending: true, status: 200, calls: 1},
		{name: "zero byte task", status: 200, calls: 1, change: func(task *domain.UploadTask) { task.FileSize, task.ReservedBytes = 0, 0 }},
		{name: "other source device", status: 200, calls: 1, change: func(task *domain.UploadTask) { task.DeviceID = 15 }},
		{name: "public error summary", status: 200, calls: 1, change: func(task *domain.UploadTask) {
			code := "CHECKSUM_MISMATCH"
			task.LastErrorCode = &code
		}},
		{name: "maximum identifiers", path: "18446744073709551615", taskID: ^uint64(0), maximumActor: true, status: 200, calls: 1},
		{name: "missing actor", missingActor: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "zero id", path: "0", status: 400, code: "INVALID_ARGUMENT"},
		{name: "leading zero", path: "019", status: 400, code: "INVALID_ARGUMENT"},
		{name: "overflow", path: "18446744073709551616", status: 400, code: "INVALID_ARGUMENT"},
		{name: "double encoded path", path: "%2531%2539", status: 400, code: "INVALID_ARGUMENT"},
		{name: "forbidden origin", forbiddenOrigin: true, status: 403, code: "CORS_FORBIDDEN"},
		{name: "missing task", serviceError: domain.ErrUploadTaskNotFound, status: 404, calls: 1, code: "UPLOAD_NOT_FOUND"},
		{name: "wrapped missing task", serviceError: fmt.Errorf("private-task-query: %w", domain.ErrUploadTaskNotFound), status: 404, calls: 1, code: "UPLOAD_NOT_FOUND"},
		{name: "disabled account", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "revoked session", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing session", serviceError: domain.ErrSessionNotFound, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "expired session", serviceError: domain.ErrSessionExpired, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "revoked device", serviceError: domain.ErrDeviceRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing account", serviceError: domain.ErrAccountNotFound, status: 401, calls: 1, code: "AUTH_REQUIRED"},
		{name: "expired token", serviceError: domain.ErrAccessTokenExpired, status: 401, calls: 1, code: "TOKEN_EXPIRED"},
		{name: "invalid token", serviceError: domain.ErrInvalidAccessToken, status: 401, calls: 1, code: "AUTH_REQUIRED"},
		{name: "data error", serviceError: domain.ErrInvalidUploadTaskData, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "query error", serviceError: errors.New("private-task-query"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "nil result", nilResult: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "wrong target", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(task *domain.UploadTask) { task.ID = 20 }},
		{name: "foreign result", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(task *domain.UploadTask) { task.UserID = 8 }},
		{name: "invalid status", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(task *domain.UploadTask) { task.Status = "UNKNOWN" }},
		{name: "unsafe bytes", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(task *domain.UploadTask) { task.FileSize = 1 << 53 }},
		{name: "invalid time", status: 500, calls: 1, code: "INTERNAL_ERROR", change: func(task *domain.UploadTask) { task.CreatedAt = time.Time{} }},
		{name: "pending path error", path: "0", pending: true, status: 400, code: "INVALID_ARGUMENT"},
		{name: "pending missing task", pending: true, serviceError: domain.ErrUploadTaskNotFound, status: 404, calls: 1, code: "UPLOAD_NOT_FOUND"},
		{name: "pending auth error", pending: true, serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "cancelled before handler", cancelBefore: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled after service", cancelAfter: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled with missing task", cancelAfter: true, serviceError: domain.ErrUploadTaskNotFound, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled at delivery", cancelOn: 4, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled before write", cancelOn: 5, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service deadline", deadline: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service panic", panicService: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 传入可信 Actor，核对查询目标及原上下文的交接。
			actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 11}
			result := uploadTaskSuccessFixture()
			if fixture.maximumActor {
				maximum := ^uint64(0)
				actor = domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
				result.ID, result.UserID, result.DeviceID = maximum, maximum, maximum
			}
			if fixture.change != nil {
				fixture.change(result)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fixture.cancelBefore {
				cancel()
			}
			if fixture.deadline {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
			}
			targetID := fixture.taskID
			if targetID == 0 {
				targetID = 19
			}
			var expectedContext context.Context
			var logs bytes.Buffer
			calls, downstream := 0, 0
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			query := handler.UploadTask(func(received context.Context, identity domain.Actor, target uint64) (*domain.UploadTask, error) {
				calls++
				if received != expectedContext || identity != actor || target != targetID {
					t.Error("任务查询上下文、身份或路径目标错误")
				}
				if fixture.deadline {
					<-received.Done()
					return nil, received.Err()
				}
				if fixture.cancelAfter {
					cancel()
				}
				if fixture.panicService {
					panic("private-task-panic")
				}
				if fixture.nilResult {
					return nil, nil
				}
				return result, fixture.serviceError
			})
			router.GET("/test-upload-task/:id", func(requestContext *gin.Context) {
				if !fixture.missingActor {
					requestContext.Request = requestContext.Request.WithContext(requestcontext.WithActor(requestContext.Request.Context(), actor))
				}
				if fixture.cancelOn != 0 {
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: requestContext.Request.Context(), cancel: cancel, cancelOn: fixture.cancelOn})
				}
				if fixture.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				expectedContext = requestContext.Request.Context()
			}, query, func(*gin.Context) { downstream++ })
			path := fixture.path
			if path == "" {
				path = "19"
			}
			request := httptest.NewRequest("GET", "/test-upload-task/"+path+"?id=99&user_id=99&note=private-query", strings.NewReader(`{"id":"99","user_id":"99"}`)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer private-access")
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			if !fixture.noOrigin {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
			}
			if fixture.forbiddenOrigin {
				request.Header.Set("Origin", "https://other.example")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对成功/错误交付及正文、缓存和认证头。
			if response.Code != fixture.status || calls != fixture.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Encoding") != "" || len(response.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("任务查询状态、调用顺序或响应头错误", response.Code, calls, downstream)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || (fixture.code != "CORS_FORBIDDEN" && response.Header().Get("Pragma") != "no-cache") {
				t.Fatal("任务请求 ID 或缓存头错误")
			}
			if !fixture.noOrigin && !fixture.forbiddenOrigin && response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("任务查询 CORS 头错误")
			}
			if fixture.status == 200 {
				var body dto.UploadTaskResponse
				expected, err := dto.NewUploadTaskData(result)
				if err != nil || json.Unmarshal(response.Body.Bytes(), &body) != nil || !reflect.DeepEqual(body.Data, *expected) || body.Data.ID != strconv.FormatUint(targetID, 10) ||
					body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
					t.Fatal("任务查询成功 JSON 错误", err)
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != fixture.code || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Content-Length") != "" {
					t.Fatal("任务查询统一错误结构错误")
				}
				if fixture.status == 400 {
					if len(body.Error.Details) != 1 || body.Error.Details["field"] != "id" || body.Error.Message != "上传任务 ID 须为 1～18446744073709551615 的标准十进制整数" {
						t.Fatal("任务查询路径字段错误")
					}
				} else if body.Error.Details != nil {
					t.Fatal("任务查询非字段错误包含额外详情")
				}
				if fixture.status == 404 && body.Error.Message != "上传任务不存在" {
					t.Fatal("任务缺失提示错误")
				}
			}
			if (fixture.status == 401) != (response.Header().Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) {
				t.Fatal("任务认证提示与状态不符")
			}
			// 3. 请求正文、凭证、查询和内部错误各自遵守公开响应及日志边界。
			for _, marker := range []string{"private-", "user_id", "\"id\":\"99\""} {
				if strings.Contains(response.Body.String(), marker) {
					t.Fatal("任务响应包含请求原文或内部信息")
				}
			}
			for _, marker := range []string{"private-query", "private-access", "private-refresh", "private-task", result.RequestedName} {
				if strings.Contains(logs.String(), marker) {
					t.Fatal("任务完成日志包含请求或任务原文")
				}
			}
		})
	}
}

func TestUploadTaskHandlerAuthorization(t *testing.T) {
	for _, mode := range []string{"missing bearer", "cookie only", "query only", "invalid bearer"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-upload-task/:id", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }, handler.AuthorizationFailure), handler.UploadTask(func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) {
				calls++
				return nil, nil
			}))
			request := httptest.NewRequest("GET", "/test-upload-task/0", nil)
			switch mode {
			case "cookie only":
				request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			case "query only":
				request.URL.RawQuery += "&access_token=private-access"
			case "invalid bearer":
				request.Header.Set("Authorization", "Bearer private-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 401 || body.Error.Code != "AUTH_REQUIRED" || body.Error.Details != nil || calls != 0 || body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("认证未先于上传任务路径校验", calls)
			}
		})
	}
}

func TestUploadTaskHandlerCommittedResponse(t *testing.T) {
	for _, mode := range []string{"body", "headers"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET("/test-upload-task/:id", func(requestContext *gin.Context) {
				if mode == "body" {
					requestContext.String(202, "already-written")
				} else {
					requestContext.Status(202)
					requestContext.Writer.WriteHeaderNow()
				}
			}, handler.UploadTask(func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error) {
				calls++
				return nil, nil
			}), func(*gin.Context) { t.Error("已提交响应进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("GET", "/test-upload-task/23", nil))
			expected := ""
			if mode == "body" {
				expected = "already-written"
			}
			if response.Code != 202 || response.Body.String() != expected || calls != 0 {
				t.Fatal("已提交响应被修改或调用了服务")
			}
		})
	}
}

func TestUploadTaskHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少上传任务服务仍构造成功")
		}
	}()
	handler.UploadTask(nil)
}
