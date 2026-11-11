package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRequireAuthorizationResponses(t *testing.T) {
	validActor := &domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
	for _, testCase := range []struct {
		name, code, message string
		actor               *domain.Actor
		err                 error
		headers             []string
		status, calls       int
		before, during      bool
		panicValue          bool
	}{
		{name: "success", actor: validActor, status: 204, calls: 1},
		{name: "max ids", actor: &domain.Actor{UserID: math.MaxUint64, SessionID: math.MaxUint64, DeviceID: math.MaxUint64}, status: 204, calls: 1},
		{name: "missing header", headers: []string{}, status: 401, code: "AUTH_REQUIRED", message: "请登录后重试"},
		{name: "duplicate header", headers: []string{"Bearer private-token", "Bearer private-token"}, status: 401, code: "AUTH_REQUIRED", message: "请登录后重试"},
		{name: "malformed header", headers: []string{"Basic private-token"}, status: 401, code: "AUTH_REQUIRED", message: "请登录后重试"},
		{name: "invalid token", err: domain.ErrInvalidAccessToken, status: 401, code: "AUTH_REQUIRED", message: "请登录后重试", calls: 1},
		{name: "missing current account", err: domain.ErrAccountNotFound, status: 401, code: "AUTH_REQUIRED", message: "请登录后重试", calls: 1},
		{name: "expired token", err: domain.ErrAccessTokenExpired, status: 401, code: "TOKEN_EXPIRED", message: "访问令牌已过期，请刷新后重试", calls: 1},
		{name: "missing session", err: domain.ErrSessionNotFound, status: 401, code: "SESSION_REVOKED", message: "登录会话已失效，请重新登录", calls: 1},
		{name: "expired session", err: domain.ErrSessionExpired, status: 401, code: "SESSION_REVOKED", message: "登录会话已失效，请重新登录", calls: 1},
		{name: "revoked session", err: domain.ErrSessionRevoked, status: 401, code: "SESSION_REVOKED", message: "登录会话已失效，请重新登录", calls: 1},
		{name: "revoked device", err: domain.ErrDeviceRevoked, status: 401, code: "SESSION_REVOKED", message: "登录会话已失效，请重新登录", calls: 1},
		{name: "disabled account", err: domain.ErrAccountDisabled, status: 403, code: "ACCOUNT_DISABLED", message: "账号已被禁用", calls: 1},
		{name: "wrapped expired token", err: fmt.Errorf("private-wrapped: %w", domain.ErrAccessTokenExpired), status: 401, code: "TOKEN_EXPIRED", message: "访问令牌已过期，请刷新后重试", calls: 1},
		{name: "internal fault", err: errors.New("private-database-password"), status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "invalid account data", err: domain.ErrInvalidAccountData, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "invalid session data", err: domain.ErrInvalidSessionData, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "cancellation error", err: context.Canceled, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "deadline error", err: context.DeadlineExceeded, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "nil successful result", status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "zero user", actor: &domain.Actor{SessionID: 11, DeviceID: 13}, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "zero session", actor: &domain.Actor{UserID: 7, DeviceID: 13}, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "zero device", actor: &domain.Actor{UserID: 7, SessionID: 11}, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "error with identity", actor: validActor, err: domain.ErrSessionRevoked, status: 401, code: "SESSION_REVOKED", message: "登录会话已失效，请重新登录", calls: 1},
		{name: "already canceled", before: true, status: 500, code: "INTERNAL_ERROR"},
		{name: "canceled during authorization", during: true, actor: validActor, status: 500, code: "INTERNAL_ERROR", calls: 1},
		{name: "authorization panic", panicValue: true, status: 500, code: "INTERNAL_ERROR", calls: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var logs bytes.Buffer
			authorizationCalls, handlerCalls := 0, 0
			request := httptest.NewRequest(http.MethodGet, "/test-auth?token=private-query", nil)
			ctx, cancel := context.WithCancel(request.Context())
			defer cancel()
			request = request.WithContext(ctx)
			if testCase.before {
				cancel()
			}
			headers := testCase.headers
			if headers == nil {
				headers = []string{"Bearer private-token"}
			}
			for _, value := range headers {
				request.Header.Add("Authorization", value)
			}
			request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("Cookie", "session=private-cookie")
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(ctx context.Context, tokenText string) (*domain.Actor, error) {
				authorizationCalls++
				if tokenText != "private-token" || requestcontext.RequestID(ctx) == "" || ctx.Done() != request.Context().Done() {
					t.Error("授权服务未收到原凭证及请求上下文")
				}
				if testCase.panicValue {
					panic("private-authorization-panic")
				}
				if testCase.during {
					cancel()
				}
				return testCase.actor, testCase.err
			}, handler.AuthorizationFailure)
			router.GET("/test-auth", protected, func(requestContext *gin.Context) {
				handlerCalls++
				actor, exists := requestcontext.Actor(requestContext.Request.Context())
				if !exists || testCase.actor == nil || actor != *testCase.actor {
					t.Error("下游未得到授权身份")
				}
				requestContext.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authorizationCalls != testCase.calls {
				t.Fatalf("状态或授权次数错误 status=%d calls=%d", response.Code, authorizationCalls)
			}
			if response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
				t.Fatal("认证响应丢失 CORS 放行头")
			}
			if testCase.status == 204 {
				if handlerCalls != 1 || response.Body.Len() != 0 {
					t.Fatal("成功请求未恰好执行一次下游处理")
				}
			} else {
				if handlerCalls != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
					t.Fatal("拒绝请求继续执行或响应头异常")
				}
				var body map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
					t.Fatal("错误正文无效")
				}
				detail, valid := body["error"].(map[string]any)
				message := testCase.message
				if message == "" {
					message = "服务内部错误，请稍后重试"
				}
				details, hasDetails := detail["details"]
				if !valid || len(body) != 2 || len(detail) != 3 || detail["code"] != testCase.code || detail["message"] != message || !hasDetails || details != nil || body["request_id"] != response.Header().Get("X-Request-ID") {
					t.Fatal("错误正文不符合固定契约")
				}
			}
			challenge := response.Header().Get("WWW-Authenticate")
			if testCase.status == 401 && challenge != `Bearer realm="cloudnest"` || testCase.status != 401 && challenge != "" {
				t.Fatal("认证提示头不符合状态约定")
			}
			if _, exists := requestcontext.Actor(request.Context()); exists {
				t.Fatal("中间件修改了原请求的父上下文")
			}
			if strings.Contains(response.Body.String(), "private-") || strings.Contains(logs.String(), "private-") {
				t.Fatal("响应或日志泄漏原始输入或异常")
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil || record["status"] != float64(testCase.status) || record["request_id"] != response.Header().Get("X-Request-ID") {
				t.Fatal("完成日志与响应不一致")
			}
		})
	}
}

func TestRequireAuthorizationCORSAndPublicRoutes(t *testing.T) {
	calls := 0
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	protected := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
		calls++
		return nil, domain.ErrInvalidAccessToken
	}, handler.AuthorizationFailure)
	router.GET("/test-auth", protected, func(*gin.Context) { t.Error("拒绝请求进入下游") })
	for _, testCase := range []struct {
		name, method, path, origin string
		status                     int
		preflight                  bool
	}{
		{name: "preflight", method: "OPTIONS", path: "/test-auth", origin: "http://127.0.0.1:5173", status: 204, preflight: true},
		{name: "unknown origin", method: "GET", path: "/test-auth", origin: "https://unknown.example", status: 403},
		{name: "ping", method: "GET", path: "/ping", status: 200},
		{name: "live", method: "GET", path: "/health/live", status: 200},
		{name: "ready", method: "GET", path: "/health/ready", status: 200},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.path, nil)
			if testCase.origin != "" {
				request.Header.Set("Origin", testCase.origin)
			}
			if testCase.preflight {
				request.Header.Set("Access-Control-Request-Method", "GET")
				request.Header.Set("Access-Control-Request-Headers", "authorization")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || calls != 0 {
				t.Fatal("公共路由或 CORS 预处理触发了认证")
			}
		})
	}
}

func TestRequireAuthorizationConcurrentIsolation(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	protected := middleware.RequireAuthorization(func(_ context.Context, tokenText string) (*domain.Actor, error) {
		userID, err := strconv.ParseUint(tokenText, 10, 64)
		if err != nil {
			return nil, domain.ErrInvalidAccessToken
		}
		if userID%4 == 0 {
			return nil, domain.ErrSessionRevoked
		}
		return &domain.Actor{UserID: userID, SessionID: userID + 100, DeviceID: userID + 200}, nil
	}, handler.AuthorizationFailure)
	router.GET("/test-auth", protected, func(requestContext *gin.Context) {
		actor, exists := requestcontext.Actor(requestContext.Request.Context())
		if !exists || actor.SessionID != actor.UserID+100 || actor.DeviceID != actor.UserID+200 {
			t.Error("并发身份标识不一致")
		}
		requestContext.String(200, strconv.FormatUint(actor.UserID, 10))
	})
	type outcome struct {
		userID uint64
		status int
		body   string
	}
	outcomes := make(chan outcome, 32)
	for userID := uint64(1); userID <= 32; userID++ {
		go func() {
			request := httptest.NewRequest("GET", "/test-auth", nil)
			request.Header.Set("Authorization", "Bearer "+strconv.FormatUint(userID, 10))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			outcomes <- outcome{userID: userID, status: response.Code, body: response.Body.String()}
		}()
	}
	for range 32 {
		result := <-outcomes
		if result.userID%4 == 0 {
			if result.status != 401 || !strings.Contains(result.body, `"code":"SESSION_REVOKED"`) {
				t.Error("并发失效会话未被拒绝")
			}
		} else if result.status != 200 || result.body != strconv.FormatUint(result.userID, 10) {
			t.Error("并发请求身份出现串号")
		}
	}
}

func TestRequireAuthorizationHEAD(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "invalid", err: domain.ErrInvalidAccessToken, status: 401},
		{name: "expired", err: domain.ErrAccessTokenExpired, status: 401},
		{name: "revoked", err: domain.ErrSessionRevoked, status: 401},
		{name: "disabled", err: domain.ErrAccountDisabled, status: 403},
		{name: "internal", err: errors.New("private-head-fault"), status: 500},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				return nil, testCase.err
			}, handler.AuthorizationFailure)
			router.HEAD("/test-auth", protected, func(*gin.Context) { t.Error("失败 HEAD 请求进入下游") })
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			request, err := http.NewRequest(http.MethodHead, server.URL+"/test-auth", nil)
			if err != nil {
				t.Fatal("HEAD 测试请求构造失败")
			}
			request.Header.Set("Authorization", "Bearer private-token")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal("HEAD 测试请求失败")
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			if readError != nil || closeError != nil || len(body) != 0 || response.StatusCode != testCase.status || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("HEAD 状态、缓存或正文语义异常")
			}
		})
	}
}
