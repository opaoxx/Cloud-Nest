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

func TestRevokeDeviceHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, path, code, field                            string
		status, calls, cancelOn                            int
		targetID                                           uint64
		serviceError                                       error
		missingActor, nilResult, unconfirmed, maximumActor bool
		cancelBefore, cancelAfter, deadline, panicService  bool
		noOrigin, forbiddenOrigin, pending                 bool
	}{
		{name: "another device", status: 200, calls: 1},
		{name: "current device", path: "13", targetID: 13, status: 200, calls: 1},
		{name: "maximum target", path: "18446744073709551615", targetID: ^uint64(0), status: 200, calls: 1},
		{name: "maximum actor", maximumActor: true, status: 200, calls: 1},
		{name: "encoded target", path: "%31%39", status: 200, calls: 1},
		{name: "no origin", noOrigin: true, status: 200, calls: 1},
		{name: "pending success headers", pending: true, status: 200, calls: 1},
		{name: "missing actor", missingActor: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "zero target", path: "0", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "leading zero", path: "019", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "overflow target", path: "18446744073709551616", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "invalid target", path: "invalid-device", status: 400, code: "INVALID_ARGUMENT", field: "id"},
		{name: "forbidden origin", forbiddenOrigin: true, status: 403, code: "CORS_FORBIDDEN"},
		{name: "missing target", serviceError: domain.ErrTargetDeviceNotFound, status: 404, calls: 1, code: "DEVICE_NOT_FOUND"},
		{name: "wrapped target error", serviceError: fmt.Errorf("private-target-fault: %w", domain.ErrTargetDeviceNotFound), status: 404, calls: 1, code: "DEVICE_NOT_FOUND"},
		{name: "pending target error headers", pending: true, serviceError: domain.ErrTargetDeviceNotFound, status: 404, calls: 1, code: "DEVICE_NOT_FOUND"},
		{name: "missing caller session", serviceError: domain.ErrSessionNotFound, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "revoked caller session", serviceError: domain.ErrSessionRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "expired caller session", serviceError: domain.ErrSessionExpired, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "revoked caller device", serviceError: domain.ErrDeviceRevoked, status: 401, calls: 1, code: "SESSION_REVOKED"},
		{name: "missing caller device", serviceError: domain.ErrDeviceNotFound, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "missing account", serviceError: domain.ErrAccountNotFound, status: 401, calls: 1, code: "AUTH_REQUIRED"},
		{name: "disabled account", serviceError: domain.ErrAccountDisabled, status: 403, calls: 1, code: "ACCOUNT_DISABLED"},
		{name: "database failure", serviceError: errors.New("private-database-fault"), status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "nil result", nilResult: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "unconfirmed result", unconfirmed: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled at entrance", cancelBefore: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled before service", cancelOn: 2, status: 500, code: "INTERNAL_ERROR"},
		{name: "cancelled after service", cancelAfter: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled before target error", cancelAfter: true, serviceError: domain.ErrTargetDeviceNotFound, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled at success delivery", cancelOn: 4, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "cancelled before response write", cancelOn: 5, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service deadline", deadline: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
		{name: "service panic", panicService: true, status: 500, calls: 1, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试装配身份，检查处理函数传给服务的上下文、Actor 和目标。
			actor := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
			if testCase.maximumActor {
				actor = domain.Actor{UserID: ^uint64(0), SessionID: ^uint64(0), DeviceID: ^uint64(0)}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.deadline {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
			}
			if testCase.cancelBefore {
				cancel()
			}
			targetID := testCase.targetID
			if targetID == 0 {
				targetID = 19
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			calls, downstream := 0, 0
			var expectedContext context.Context
			revoke := handler.RevokeDevice(func(received context.Context, identity domain.Actor, target uint64) (*domain.RevokeDeviceResult, error) {
				calls++
				if received != expectedContext || identity != actor || target != targetID {
					t.Error("服务收到的上下文、Actor 或目标不准确")
				}
				if testCase.deadline {
					<-received.Done()
					return nil, received.Err()
				}
				if testCase.cancelAfter {
					cancel()
				}
				if testCase.panicService {
					panic("private-service-panic")
				}
				if testCase.nilResult {
					return nil, nil
				}
				return &domain.RevokeDeviceResult{Revoked: !testCase.unconfirmed}, testCase.serviceError
			})
			router.DELETE("/test-revoke-device/:id", func(requestContext *gin.Context) {
				if !testCase.missingActor {
					requestContext.Request = requestContext.Request.WithContext(requestcontext.WithActor(requestContext.Request.Context(), actor))
				}
				if testCase.cancelOn != 0 {
					requestContext.Request = requestContext.Request.WithContext(&cancelUserSessionsContext{Context: requestContext.Request.Context(), cancel: cancel, cancelOn: testCase.cancelOn})
				}
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				expectedContext = requestContext.Request.Context()
			}, revoke, func(*gin.Context) { downstream++ })
			path := testCase.path
			if path == "" {
				path = "19"
			}
			request := httptest.NewRequest("DELETE", "/test-revoke-device/"+path+"?user_id=99&id=99&note=private-query", strings.NewReader("{\"user_id\":\"99\",\"id\":\"99\"}")).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer private-access")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			request.Header.Set("X-User-ID", "99")
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			if !testCase.noOrigin {
				request.Header.Set("Origin", "http://127.0.0.1:5173")
			}
			if testCase.forbiddenOrigin {
				request.Header.Set("Origin", "https://other.example")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			// 2. 核对交付、错误分界、字段定位及结束处理。
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Set-Cookie") != "" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatal("状态、调用次数或响应头错误", response.Code, calls, downstream)
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || (testCase.code != "CORS_FORBIDDEN" && response.Header().Get("Pragma") != "no-cache") {
				t.Fatal("请求 ID 或禁止缓存头错误")
			}
			if testCase.status == 200 {
				var body dto.RevokeDeviceResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || !body.Data.Revoked || body.RequestID != response.Header().Get("X-Request-ID") ||
					response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("Content-Encoding") != "" {
					t.Fatal("成功 JSON 或正文头错误")
				}
			} else {
				var body dto.ErrorResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("错误 JSON 或请求 ID 错误")
				}
				if testCase.field == "" {
					if body.Error.Details != nil {
						t.Fatal("非字段错误的 details 应为空")
					}
				} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != testCase.field {
					t.Fatal("路径字段错误位置不准确")
				}
				if testCase.status == 404 && (body.Error.Message != "设备不存在" || response.Header().Get("Content-Encoding") != "") {
					t.Fatal("目标 404 提示或正文编码头错误")
				}
			}
			if (testCase.status == 401) != (response.Header().Get("WWW-Authenticate") != "") {
				t.Fatal("认证提示与 HTTP 状态不匹配")
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志包含凭证、查询或内部故障原文")
			}
		})
	}
}

func TestRevokeDeviceHandlerAuthorization(t *testing.T) {
	for _, candidate := range []string{"missing bearer", "cookie only", "query only", "invalid bearer"} {
		t.Run(candidate, func(t *testing.T) {
			calls := 0
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.DELETE("/test-revoke-device/:id", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				return nil, domain.ErrInvalidAccessToken
			}, handler.AuthorizationFailure), handler.RevokeDevice(func(context.Context, domain.Actor, uint64) (*domain.RevokeDeviceResult, error) {
				calls++
				return &domain.RevokeDeviceResult{Revoked: true}, nil
			}))
			request := httptest.NewRequest("DELETE", "/test-revoke-device/0", nil)
			switch candidate {
			case "cookie only":
				request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-refresh"})
			case "query only":
				request.URL.RawQuery = "access_token=private-access"
			case "invalid bearer":
				request.Header.Set("Authorization", "Bearer invalid-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 401 || body.Error.Code != "AUTH_REQUIRED" || body.Error.Details != nil || calls != 0 ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("未认证请求未在路径校验前拒绝")
			}
		})
	}
}

func TestRevokeDeviceHandlerCommittedResponse(t *testing.T) {
	calls := 0
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.DELETE("/test-revoke-device/:id", func(requestContext *gin.Context) {
		requestContext.String(202, "already-written")
	}, handler.RevokeDevice(func(context.Context, domain.Actor, uint64) (*domain.RevokeDeviceResult, error) {
		calls++
		return &domain.RevokeDeviceResult{Revoked: true}, nil
	}), func(*gin.Context) { t.Error("已提交响应后进入下游") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("DELETE", "/test-revoke-device/0", nil))
	if response.Code != 202 || response.Body.String() != "already-written" || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || calls != 0 {
		t.Fatal("已提交响应被修改或触发服务")
	}
}

func TestRevokeDeviceHandlerDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少撤销服务仍构造成功")
		}
	}()
	handler.RevokeDevice(nil)
}
