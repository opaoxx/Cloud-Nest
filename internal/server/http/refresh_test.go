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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func refreshHandlerRequest(t *testing.T, clientType, raw string) *http.Request {
	t.Helper()
	input := dto.RefreshRequest{ClientType: clientType}
	if clientType == "native" {
		input.RefreshToken = raw
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/test-refresh?token=private-query", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(security.ClientHeader, clientType)
	request.Header.Set("Authorization", "Bearer "+raw)
	request.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
	if clientType == "web" {
		request.Header.Set("Origin", "http://127.0.0.1:5173")
		request.AddCookie(&http.Cookie{Name: security.RefreshCookieName, Value: raw})
	}
	return request
}

type cancelRefreshBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body *cancelRefreshBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	body.cancel()
	return count, err
}

func TestRefreshHandlerFlow(t *testing.T) {
	fault := errors.New("private-service-failure")
	for _, testCase := range []struct {
		name, clientType, code                                                   string
		status, calls                                                            int
		requestChange                                                            func(*http.Request)
		resultChange                                                             func(*domain.RefreshTokenResult)
		serviceError                                                             error
		secure, nilResult, cancelBefore, cancelBody, cancelService, panicService bool
	}{
		{name: "web", clientType: "web", status: 200, calls: 1},
		{name: "web secure", clientType: "web", status: 200, calls: 1, secure: true},
		{name: "native", clientType: "native", status: 200, calls: 1},
		{name: "native secure", clientType: "native", status: 200, calls: 1, secure: true},
		{name: "malformed JSON", clientType: "web", status: 400, code: "INVALID_ARGUMENT", requestChange: func(request *http.Request) { request.Body = io.NopCloser(strings.NewReader("{")) }},
		{name: "unknown field", clientType: "web", status: 400, code: "INVALID_ARGUMENT", requestChange: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader(`{"client_type":"web","extra":"private-input"}`))
		}},
		{name: "wrong media type", clientType: "web", status: 415, code: "UNSUPPORTED_MEDIA_TYPE", requestChange: func(request *http.Request) { request.Header.Set("Content-Type", "text/plain") }},
		{name: "body too large", clientType: "web", status: 413, code: "REQUEST_BODY_TOO_LARGE", requestChange: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", int(binding.MaxJSONBodyBytes)+1)))
		}},
		{name: "unknown client", clientType: "other", status: 400, code: "INVALID_ARGUMENT"},
		{name: "web body token", clientType: "web", status: 400, code: "INVALID_ARGUMENT", requestChange: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader(`{"client_type":"web","refresh_token":"private-body"}`))
		}},
		{name: "native missing body token", clientType: "native", status: 400, code: "INVALID_ARGUMENT", requestChange: func(request *http.Request) {
			request.Body = io.NopCloser(strings.NewReader(`{"client_type":"native"}`))
		}},
		{name: "missing client header", clientType: "web", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Del(security.ClientHeader) }},
		{name: "duplicate client header", clientType: "web", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Add(security.ClientHeader, "web") }},
		{name: "mismatched client header", clientType: "web", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Set(security.ClientHeader, "native") }},
		{name: "web missing origin", clientType: "web", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Del("Origin") }},
		{name: "native with origin", clientType: "native", status: 403, code: "CLIENT_SOURCE_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Set("Origin", "http://127.0.0.1:5173") }},
		{name: "unknown origin", clientType: "web", status: 403, code: "CORS_FORBIDDEN", requestChange: func(request *http.Request) { request.Header.Set("Origin", "https://other.example") }},
		{name: "missing web cookie", clientType: "web", status: 401, code: "AUTH_REQUIRED", requestChange: func(request *http.Request) { request.Header.Del("Cookie") }},
		{name: "duplicate web cookie", clientType: "web", status: 401, code: "AUTH_REQUIRED", requestChange: func(request *http.Request) { request.Header.Add("Cookie", security.RefreshCookieName+"=private-other") }},
		{name: "invalid credential", clientType: "web", status: 401, code: "AUTH_REQUIRED", calls: 1, serviceError: domain.ErrInvalidRefreshToken},
		{name: "wrapped invalid credential", clientType: "native", status: 401, code: "AUTH_REQUIRED", calls: 1, serviceError: fmt.Errorf("wrapped: %w", domain.ErrInvalidRefreshToken)},
		{name: "revoked session", clientType: "web", status: 401, code: "SESSION_REVOKED", calls: 1, serviceError: domain.ErrSessionRevoked},
		{name: "revoked device", clientType: "web", status: 401, code: "SESSION_REVOKED", calls: 1, serviceError: domain.ErrDeviceRevoked},
		{name: "expired session", clientType: "web", status: 401, code: "SESSION_REVOKED", calls: 1, serviceError: domain.ErrSessionExpired},
		{name: "disabled account", clientType: "web", status: 403, code: "ACCOUNT_DISABLED", calls: 1, serviceError: domain.ErrAccountDisabled},
		{name: "digest conflict", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, serviceError: domain.ErrRefreshTokenConflict},
		{name: "service failure", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, serviceError: fault},
		{name: "nil result", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, nilResult: true},
		{name: "invalid result", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, resultChange: func(result *domain.RefreshTokenResult) { result.Session.UserID = 0 }},
		{name: "expired output", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, resultChange: func(result *domain.RefreshTokenResult) { result.AccessToken.ExpiresAt = time.Now().Add(-time.Second) }},
		{name: "invalid output cookie", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, resultChange: func(result *domain.RefreshTokenResult) { result.RefreshToken = "private-invalid;cookie" }},
		{name: "canceled before reading", clientType: "web", status: 500, code: "INTERNAL_ERROR", cancelBefore: true},
		{name: "canceled during reading", clientType: "web", status: 500, code: "INTERNAL_ERROR", cancelBody: true},
		{name: "canceled after service", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, cancelService: true},
		{name: "service panic", clientType: "web", status: 500, code: "INTERNAL_ERROR", calls: 1, panicService: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			const original = "private-old-refresh"
			result := refreshSuccessFixture()
			if testCase.resultChange != nil {
				testCase.resultChange(result)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := refreshHandlerRequest(t, testCase.clientType, original).WithContext(ctx)
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
			policy := security.NewLoginSourcePolicy([]string{"http://127.0.0.1:5173"})
			calls, downstream := 0, 0
			var expectedContext context.Context
			refresh := handler.Refresh(policy, func(received context.Context, raw string) (*domain.RefreshTokenResult, error) {
				calls++
				if received != expectedContext || raw != original {
					t.Error("服务未收到请求上下文及提取的原文")
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
				return result, testCase.serviceError
			}, testCase.secure)
			router.POST("/test-refresh", func(requestContext *gin.Context) { expectedContext = requestContext.Request.Context() }, refresh, func(*gin.Context) { downstream++ })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || calls != testCase.calls || downstream != 0 {
				t.Fatal("状态、服务调用或中止行为错误", response.Code, calls, downstream)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Error("响应缺少 no-store")
			}
			if testCase.code != "CORS_FORBIDDEN" && response.Header().Get("Pragma") != "no-cache" {
				t.Error("处理函数响应缺少 no-cache")
			}
			if response.Header().Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" {
				t.Fatal("请求 ID 错误")
			}
			if testCase.status == 200 {
				var body dto.RefreshResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != response.Header().Get("X-Request-ID") || body.Data.AccessToken != result.AccessToken.Token {
					t.Fatal("成功响应结构错误", err)
				}
				cookies := response.Result().Cookies()
				if testCase.clientType == "web" {
					if len(cookies) != 1 || cookies[0].Value != result.RefreshToken || cookies[0].Secure != testCase.secure || body.Data.RefreshToken != "" {
						t.Fatal("Web 凭证交付错误")
					}
				} else if len(cookies) != 0 || body.Data.RefreshToken != result.RefreshToken {
					t.Fatal("原生凭证交付错误")
				}
			} else {
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Set-Cookie") != "" {
					t.Fatal("错误结构或 Cookie 处理异常", err)
				}
				if strings.Contains(response.Body.String(), "private-") || strings.Contains(response.Body.String(), "fixture-") || strings.Contains(response.Body.String(), result.RefreshToken) {
					t.Error("失败正文包含内部凭证或错误")
				}
			}
			if strings.Contains(logs.String(), original) || strings.Contains(logs.String(), result.RefreshToken) || strings.Contains(logs.String(), result.AccessToken.Token) || strings.Contains(logs.String(), "private-") {
				t.Error("完成日志包含敏感数据")
			}
		})
	}
}

func TestRefreshHandlerCommittedResponse(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	policy := security.NewLoginSourcePolicy(nil)
	refresh := handler.Refresh(policy, func(context.Context, string) (*domain.RefreshTokenResult, error) {
		t.Error("响应已提交仍调用刷新服务")
		return nil, nil
	}, false)
	router.POST("/test-refresh", func(requestContext *gin.Context) { requestContext.String(202, "already-written") }, refresh,
		func(*gin.Context) { t.Error("响应已提交后执行下游") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, refreshHandlerRequest(t, "native", "private-old-refresh"))
	if response.Code != 202 || response.Body.String() != "already-written" || response.Header().Get("Set-Cookie") != "" {
		t.Fatal("已提交响应被覆盖或追加")
	}
}

func TestRefreshHandlerDependencies(t *testing.T) {
	for _, missing := range []string{"policy", "service"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖时仍构造成功")
				}
			}()
			policy := security.NewLoginSourcePolicy(nil)
			refresh := func(context.Context, string) (*domain.RefreshTokenResult, error) { return nil, nil }
			if missing == "policy" {
				policy = nil
			} else {
				refresh = nil
			}
			handler.Refresh(policy, refresh, false)
		})
	}
}
