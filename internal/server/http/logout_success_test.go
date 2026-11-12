package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestLogoutSuccessDelivery(t *testing.T) {
	for _, testCase := range []struct {
		name, clientType string
		secure, pending  bool
	}{
		{name: "web http", clientType: "web"},
		{name: "web secure", clientType: "web", secure: true},
		{name: "native http", clientType: "native"},
		{name: "native secure", clientType: "native", secure: true},
		{name: "pending headers", clientType: "web", pending: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := &domain.LogoutResult{Revoked: true}
			var logs bytes.Buffer
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
			router.POST("/test-logout-success", func(requestContext *gin.Context) {
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.LogoutSuccess(requestContext, result, testCase.clientType, testCase.secure)
			}, func(*gin.Context) { t.Error("成功交付后进入下游") })
			request := httptest.NewRequest("POST", "/test-logout-success?token=fixture-query-secret", nil)
			request.Header.Set("Origin", origin)
			request.Header.Set("X-Request-ID", "28ee758a-0727-42de-ae7d-3bb33155e148")
			request.Header.Set("Authorization", "Bearer fixture-access-secret")
			request.Header.Set("X-Forwarded-Proto", "https")
			request.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "fixture-refresh-secret"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("Content-Encoding") != "" {
				t.Fatal("成功状态或响应头错误")
			}
			var body dto.LogoutResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !body.Data.Revoked || body.RequestID != request.Header.Get("X-Request-ID") ||
				body.RequestID != response.Header().Get("X-Request-ID") || !strings.Contains(logs.String(), body.RequestID) {
				t.Fatal("退出正文、响应头或日志请求 ID 错误", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil || len(fields) != 2 || string(fields["data"]) != `{"revoked":true}` {
				t.Fatal("退出响应包含非约定字段", err)
			}
			cookies := response.Result().Cookies()
			if testCase.clientType == "web" {
				if len(cookies) != 1 {
					t.Fatal("Web 响应未设置单个清理 Cookie")
				}
				cookie := cookies[0]
				if cookie.Name != handler.RefreshCookieName || cookie.Value != "" || cookie.Path != handler.RefreshCookiePath || cookie.Domain != "" || !cookie.HttpOnly ||
					cookie.SameSite != http.SameSiteStrictMode || cookie.Secure != testCase.secure || cookie.MaxAge != -1 || !cookie.Expires.Equal(time.Unix(1, 0)) ||
					!strings.Contains(response.Header().Get("Set-Cookie"), "Max-Age=0") {
					t.Fatal("清理 Cookie 的名称、路径、期限或安全属性错误")
				}
			} else if len(cookies) != 0 {
				t.Fatal("原生交付设置了 Web Cookie")
			}
			for _, secret := range []string{"fixture-query-secret", "fixture-access-secret", "fixture-refresh-secret"} {
				if strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) || strings.Contains(response.Header().Get("Set-Cookie"), secret) {
					t.Fatal("正文、日志或 Cookie 包含请求中的凭证")
				}
			}
			if !result.Revoked {
				t.Fatal("交付修改了服务结果")
			}
		})
	}
}

func TestLogoutSuccessFailures(t *testing.T) {
	for _, failure := range []string{"nil result", "unconfirmed result", "empty client", "invalid client", "uppercase client", "padded client", "canceled", "cancel before write"} {
		t.Run(failure, func(t *testing.T) {
			result := &domain.LogoutResult{Revoked: true}
			clientType := "web"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "nil result":
				result = nil
			case "unconfirmed result":
				result.Revoked = false
			case "empty client":
				clientType = ""
			case "invalid client":
				clientType = "other"
			case "uppercase client":
				clientType = "WEB"
			case "padded client":
				clientType = " web "
			case "canceled":
				cancel()
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/test-logout-success", func(requestContext *gin.Context) {
				if failure == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					// 复用刷新交付测试的上下文，在第二次 Err 检查时取消。
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.LogoutSuccess(requestContext, result, clientType, false)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("POST", "/test-logout-success", nil).WithContext(ctx))
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Set-Cookie") != "" ||
				response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
				t.Fatal("失败交付的错误结构、请求 ID 或响应头异常", err)
			}
		})
	}
}

func TestLogoutSuccessCommittedResponse(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-logout-success", func(requestContext *gin.Context) {
		requestContext.String(202, "already-written")
		handler.LogoutSuccess(requestContext, &domain.LogoutResult{Revoked: true}, "web", false)
	}, func(*gin.Context) { t.Error("已提交响应后进入下游") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/test-logout-success", nil))
	if response.Code != 202 || response.Body.String() != "already-written" || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		response.Header().Get("Set-Cookie") != "" || response.Header().Get("Cache-Control") != "" {
		t.Fatal("已提交响应被覆盖或追加 Cookie")
	}
}

func TestLogoutSuccessCookieJar(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "http"
		if secure {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			// 1. 测试路由复用来源校验及登录、刷新、退出的成功交付方法。
			loginResult, profile := loginSuccessFixture()
			refreshResult := &domain.RefreshTokenResult{Session: loginResult.Session, TokenGeneration: 2, AccessToken: loginResult.AccessToken, RefreshToken: "fixture-rotated-refresh"}
			origin := "http://127.0.0.1:5173"
			policy := security.NewLoginSourcePolicy([]string{origin})
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			router.POST("/api/v1/auth/test-cookie/:operation", func(requestContext *gin.Context) {
				clientType := "web"
				operation := requestContext.Param("operation")
				if operation == "native" {
					clientType = "native"
				}
				if !policy.Allows(requestContext.Request, clientType) {
					handler.ClientSourceForbidden(requestContext)
					return
				}
				switch operation {
				case "login":
					handler.LoginSuccess(requestContext, loginResult, profile, "web", secure)
				case "refresh":
					handler.RefreshSuccess(requestContext, refreshResult, "web", secure)
				case "failure":
					handler.LogoutSuccess(requestContext, nil, "web", secure)
				default:
					handler.LogoutSuccess(requestContext, &domain.LogoutResult{Revoked: true}, clientType, secure)
				}
			})
			var server *httptest.Server
			if secure {
				server = httptest.NewTLSServer(router)
			} else {
				server = httptest.NewServer(router)
			}
			defer server.Close()
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := server.Client()
			client.Jar = jar
			refreshURL, err := url.Parse(server.URL + "/api/v1/auth/refresh")
			if err != nil {
				t.Fatal(err)
			}
			// 2. 建立其他路径和主机的同名 Cookie，用于核对清理范围。
			outsideURL := *refreshURL
			outsideURL.Path = "/unrelated/resource"
			otherHost := *refreshURL
			otherHost.Host = "other.example"
			jar.SetCookies(&outsideURL, []*http.Cookie{{Name: handler.RefreshCookieName, Value: "unrelated-path", Path: "/unrelated"}})
			jar.SetCookies(&otherHost, []*http.Cookie{{Name: handler.RefreshCookieName, Value: "unrelated-host", Path: handler.RefreshCookiePath}})

			// 3. 登录并刷新 Cookie，检查交付失败及原生交付，再完成 Web 清理。
			for _, operation := range []string{"login", "refresh", "failure", "native", "logout", "logout"} {
				request, err := http.NewRequest("POST", server.URL+"/api/v1/auth/test-cookie/"+operation, nil)
				if err != nil {
					t.Fatal(err)
				}
				clientType := "web"
				if operation == "native" {
					clientType = "native"
				} else {
					request.Header.Set("Origin", origin)
				}
				request.Header.Set(security.ClientHeader, clientType)
				response, err := client.Do(request)
				if err != nil {
					t.Fatal("真实 Cookie 交付请求失败", operation, err)
				}
				encoded, readError := io.ReadAll(response.Body)
				_ = response.Body.Close()
				expectedStatus := 200
				if operation == "failure" {
					expectedStatus = 500
				}
				if readError != nil || response.StatusCode != expectedStatus {
					t.Fatal("网络响应异常", operation, readError)
				}
				cookies := jar.Cookies(refreshURL)
				switch operation {
				case "login":
					if len(cookies) != 1 || cookies[0].Value != loginResult.RefreshToken {
						t.Fatal("登录 Cookie 未保存")
					}
				case "refresh", "failure", "native":
					if len(cookies) != 1 || cookies[0].Value != refreshResult.RefreshToken {
						t.Fatal("刷新、失败或原生交付后的 Cookie 状态异常", operation)
					}
				case "logout":
					if len(cookies) != 0 || len(response.Cookies()) != 1 {
						t.Fatal("Web 清理未删除原认证路径 Cookie")
					}
				}
				if operation == "failure" || operation == "native" {
					if len(response.Cookies()) != 0 {
						t.Fatal("失败或原生交付修改了 Web Cookie")
					}
				}
				if operation == "logout" || operation == "native" {
					var body dto.LogoutResponse
					if json.Unmarshal(encoded, &body) != nil || !body.Data.Revoked || body.RequestID == "" || body.RequestID != response.Header.Get("X-Request-ID") {
						t.Fatal("网络退出成功 JSON 错误")
					}
				}
				if cookies := jar.Cookies(&outsideURL); len(cookies) != 1 || cookies[0].Value != "unrelated-path" {
					t.Fatal("其他路径的 Cookie 被改动")
				}
				if cookies := jar.Cookies(&otherHost); len(cookies) != 1 || cookies[0].Value != "unrelated-host" {
					t.Fatal("其他主机的 Cookie 被改动")
				}
			}
		})
	}
}
