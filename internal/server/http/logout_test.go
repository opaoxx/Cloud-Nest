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
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func logoutHandlerRequest(t *testing.T, clientType string) *http.Request {
	t.Helper()
	body, err := json.Marshal(dto.LogoutRequest{ClientType: clientType})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/test-logout?session_id=private-query", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(security.ClientHeader, clientType)
	request.Header.Set("Authorization", "Bearer private-access")
	request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
	if clientType == "web" {
		request.Header.Set("Origin", "http://127.0.0.1:5173")
	}
	return request
}

func TestLogoutHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, clientType, code                                              string
		status, calls                                                       int
		requestChange                                                       func(*http.Request)
		serviceError                                                        error
		secure, missingActor, nilResult, invalidResult                      bool
		cancelBefore, cancelBody, cancelService, panicService, maximumActor bool
	}{
		{name: "web without refresh cookie", clientType: "web", status: 200, calls: 1},
		{name: "web secure", clientType: "web", status: 200, calls: 1, secure: true},
		{name: "native", clientType: "native", status: 200, calls: 1},
		{name: "native secure", clientType: "native", status: 200, calls: 1, secure: true},
		{name: "maximum actor identifiers", clientType: "native", status: 200, calls: 1, maximumActor: true},
		{name: "missing actor", clientType: "web", status: 500, code: "INTERNAL_ERROR", missingActor: true},
		{name: "malformed json", clientType: "web", status: 400, code: "INVALID_ARGUMENT", requestChange: func(request *http.Request) { request.Body = io.NopCloser(strings.NewReader("{")) }},
		{name: "unknown session field", clientType: "web", status: 400, code: "INVALID_ARGUMENT", requestChange: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader(`{"client_type":"web","session_id":"private-session"}`))
		}},
		{name: "wrong media type", clientType: "web", status: 415, code: "UNSUPPORTED_MEDIA_TYPE", requestChange: func(request *http.Request) { request.Header.Set("Content-Type", "text/plain") }},
		{name: "body too large", clientType: "web", status: 413, code: "REQUEST_BODY_TOO_LARGE", requestChange: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", int(binding.MaxJSONBodyBytes)+1)))
		}},
		{name: "unknown client", clientType: "other", status: 400, code: "INVALID_ARGUMENT"},
		{name: "missing client header", clientType: "web", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Del(security.ClientHeader) }},
		{name: "duplicate client header", clientType: "web", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Add(security.ClientHeader, "web") }},
		{name: "mismatched client header", clientType: "web", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Set(security.ClientHeader, "native") }},
		{name: "web missing origin", clientType: "web", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Del("Origin") }},
		{name: "native with origin", clientType: "native", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Set("Origin", "http://127.0.0.1:5173") }},
		{name: "unknown origin", clientType: "web", status: 403, code: "CORS_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Set("Origin", "https://other.example") }},
		{name: "missing session", clientType: "web", status: 401, code: "SESSION_REVOKED", calls: 1, serviceError: domain.ErrSessionNotFound},
		{name: "revoked session", clientType: "web", status: 401, code: "SESSION_REVOKED", calls: 1, serviceError: domain.ErrSessionRevoked},
		{name: "wrapped revoked session", clientType: "native", status: 401, code: "SESSION_REVOKED", calls: 1, serviceError: fmt.Errorf("private-wrapper: %w", domain.ErrSessionRevoked)},
		{name: "revoked device", clientType: "web", status: 401, code: "SESSION_REVOKED", calls: 1, serviceError: domain.ErrDeviceRevoked},
		{name: "expired session", clientType: "web", status: 401, code: "SESSION_REVOKED", calls: 1, serviceError: domain.ErrSessionExpired},
		{name: "disabled account", clientType: "web", status: 403, code: "ACCOUNT_DISABLED", calls: 1, serviceError: domain.ErrAccountDisabled},
		{name: "missing account", clientType: "web", status: 401, code: "AUTH_REQUIRED", calls: 1, serviceError: domain.ErrAccountNotFound},
		{name: "service failure", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, serviceError: errors.New("private-service-failure")},
		{name: "nil result", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, nilResult: true},
		{name: "unconfirmed result", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, invalidResult: true},
		{name: "canceled before reading", clientType: "web", status: 500, code: "INTERNAL_ERROR", cancelBefore: true},
		{name: "canceled during reading", clientType: "web", status: 500, code: "INTERNAL_ERROR", cancelBody: true},
		{name: "canceled after service", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, cancelService: true},
		{name: "service panic", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, panicService: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 11}
			if testCase.maximumActor {
				actor = domain.Actor{UserID: ^uint64(0), DeviceID: ^uint64(0), SessionID: ^uint64(0)}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := logoutHandlerRequest(t, testCase.clientType).WithContext(ctx)
			request.AddCookie(&http.Cookie{Name: security.RefreshCookieName, Value: "private-refresh"})
			if testCase.name == "web without refresh cookie" {
				request.Header.Del("Cookie")
			}
			if testCase.requestChange != nil {
				testCase.requestChange(request)
			}
			if testCase.cancelBody {
				request.Body = &cancelRefreshBody{ReadCloser: request.Body, cancel: cancel}
			}
			if testCase.cancelBefore {
				cancel()
			}
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			calls, downstream := 0, 0
			var expectedContext context.Context
			logout := handler.Logout(security.NewLoginSourcePolicy([]string{"http://127.0.0.1:5173"}), func(received context.Context, receivedActor domain.Actor) (*domain.LogoutResult, error) {
				calls++
				if received != expectedContext || receivedActor != actor {
					t.Error("退出服务收到的上下文或 Actor 错误")
				}
				if testCase.cancelService {
					cancel()
				}
				if testCase.panicService {
					panic("private-panic")
				}
				if testCase.nilResult {
					return nil, nil
				}
				return &domain.LogoutResult{Revoked: !testCase.invalidResult}, testCase.serviceError
			}, testCase.secure)
			router.POST("/test-logout", func(requestContext *gin.Context) {
				if !testCase.missingActor {
					requestContext.Request = requestContext.Request.WithContext(requestcontext.WithActor(requestContext.Request.Context(), actor))
				}
				expectedContext = requestContext.Request.Context()
			}, logout, func(*gin.Context) { downstream++ })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 {
				t.Fatal("状态、服务调用次数或中止行为错误", response.Code, calls, downstream)
			}
			if response.Header().Get("Cache-Control") != "no-store" || (testCase.code != "CORS_FORBIDDEN" && response.Header().Get("Pragma") != "no-cache") {
				t.Error("响应缺少禁止缓存头")
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" {
				t.Fatal("请求 ID 错误")
			}
			if testCase.status == 200 {
				var body dto.LogoutResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !body.Data.Revoked || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("退出成功响应结构错误", err)
				}
				cookies := response.Result().Cookies()
				if testCase.clientType == "web" {
					if len(cookies) != 1 || cookies[0].Value != "" || cookies[0].MaxAge != -1 || cookies[0].Secure != testCase.secure {
						t.Fatal("Web 清理 Cookie 错误")
					}
				} else if len(cookies) != 0 {
					t.Fatal("原生退出修改了 Web Cookie")
				}
			} else {
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Set-Cookie") != "" {
					t.Fatal("统一错误响应或 Cookie 处理错误", err)
				}
				if testCase.status == 401 && response.Header().Get("WWW-Authenticate") != `Bearer realm="cloudnest"` {
					t.Fatal("认证错误缺少 Bearer 提示")
				}
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(response.Body.String(), "private-") {
				t.Error("响应或完成日志包含内部信息")
			}
		})
	}
}

func TestLogoutHandlerAuthorization(t *testing.T) {
	for _, failure := range []string{"missing bearer", "cookie only", "query only", "invalid bearer"} {
		t.Run(failure, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			authorizeCalls, logoutCalls := 0, 0
			router.POST("/test-logout", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				authorizeCalls++
				return nil, domain.ErrInvalidAccessToken
			}, handler.AuthorizationFailure), handler.Logout(security.NewLoginSourcePolicy(nil), func(context.Context, domain.Actor) (*domain.LogoutResult, error) {
				logoutCalls++
				return &domain.LogoutResult{Revoked: true}, nil
			}, false))
			// 错误正文与未认证请求同时出现，认证先于退出处理函数。
			request := httptest.NewRequest("POST", "/test-logout", strings.NewReader("{"))
			request.Header.Set("Content-Type", "application/json")
			switch failure {
			case "cookie only":
				request.AddCookie(&http.Cookie{Name: security.RefreshCookieName, Value: "private-refresh"})
			case "query only":
				request.URL.RawQuery = "access_token=private-access"
			case "invalid bearer":
				request.Header.Set("Authorization", "Bearer private-access")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 401 || body.Error.Code != "AUTH_REQUIRED" || logoutCalls != 0 || response.Header().Get("Set-Cookie") != "" {
				t.Fatal("未认证请求进入退出流程", err)
			}
			expectedAuthorizeCalls := 0
			if failure == "invalid bearer" {
				expectedAuthorizeCalls = 1
			}
			if authorizeCalls != expectedAuthorizeCalls {
				t.Fatal("授权服务调用次数错误")
			}
		})
	}
}

func TestLogoutHandlerCommittedResponse(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	logout := handler.Logout(security.NewLoginSourcePolicy(nil), func(context.Context, domain.Actor) (*domain.LogoutResult, error) {
		t.Error("响应已提交仍调用退出服务")
		return nil, nil
	}, false)
	router.POST("/test-logout", func(requestContext *gin.Context) { requestContext.String(202, "already-written") }, logout,
		func(*gin.Context) { t.Error("响应已提交后执行下游") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, logoutHandlerRequest(t, "native"))
	if response.Code != 202 || response.Body.String() != "already-written" || response.Header().Get("Set-Cookie") != "" {
		t.Fatal("已提交响应被覆盖或追加")
	}
}

func TestLogoutHandlerDependencies(t *testing.T) {
	for _, missing := range []string{"policy", "service"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖时仍构造成功")
				}
			}()
			policy := security.NewLoginSourcePolicy(nil)
			logout := func(context.Context, domain.Actor) (*domain.LogoutResult, error) { return nil, nil }
			if missing == "policy" {
				policy = nil
			} else {
				logout = nil
			}
			handler.Logout(policy, logout, false)
		})
	}
}
