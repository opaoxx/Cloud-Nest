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
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func loginSuccessFixture() (*domain.LoginTokenResult, *domain.UserProfile) {
	now := time.Now().UTC()
	refresh, _ := service.GenerateRefreshToken()
	return &domain.LoginTokenResult{
		Device:       domain.LoginDevice{ID: 13, UserID: 7, Name: "学习设备", Platform: "WEB"},
		Session:      domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13, ExpiresAt: now.Add(time.Hour).Truncate(time.Microsecond)},
		AccessToken:  domain.AccessToken{Token: "fixture-access-token", ExpiresAt: now.Add(15 * time.Minute).Truncate(time.Second)},
		RefreshToken: refresh,
	}, &domain.UserProfile{ID: 7, Username: "fixture", DisplayName: "学习用户", Status: "ACTIVE", RootEntryID: 17, QuotaBytes: 100}
}

func TestLoginSuccessDelivery(t *testing.T) {
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
			result, profile := loginSuccessFixture()
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			downstream := 0
			router.POST("/test-login-success", func(requestContext *gin.Context) {
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.LoginSuccess(requestContext, result, profile, testCase.clientType, testCase.secure)
			}, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("POST", "/test-login-success?token=fixture-query-secret", nil)
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Forwarded-Proto", "https")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != 200 || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("Content-Encoding") != "" {
				t.Fatal("成功状态、缓存、正文类型或中止行为错误")
			}
			var body dto.LoginResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Data.AccessToken != result.AccessToken.Token || body.Data.User.ID != "7" || body.Data.Session.ID != "11" ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || body.Data.ExpiresIn < 0 || body.Data.ExpiresIn > 900 {
				t.Fatal("登录正文或请求 ID 不一致")
			}
			cookies := response.Result().Cookies()
			if testCase.clientType == "web" {
				if len(cookies) != 1 {
					t.Fatal("Web 交付未设置单个刷新 Cookie")
				}
				cookie := cookies[0]
				if cookie.Name != handler.RefreshCookieName || cookie.Value != result.RefreshToken || cookie.Path != "/api/v1/auth" || cookie.Domain != "" || !cookie.HttpOnly ||
					cookie.SameSite != http.SameSiteStrictMode || cookie.Secure != testCase.secure || !cookie.Expires.Equal(result.Session.ExpiresAt.Truncate(time.Second)) ||
					body.Data.RefreshToken != "" || strings.Contains(response.Body.String(), result.RefreshToken) {
					t.Fatal("Web Cookie 属性、原文或 JSON 隐藏规则错误")
				}
			} else if len(cookies) != 0 || body.Data.RefreshToken != result.RefreshToken {
				t.Fatal("原生交付的 JSON 或 Cookie 行为错误")
			}
			if strings.Contains(logs.String(), result.RefreshToken) || strings.Contains(logs.String(), result.AccessToken.Token) || strings.Contains(logs.String(), "fixture-query-secret") {
				t.Fatal("完成日志泄漏凭证或查询参数")
			}
		})
	}
}

func TestLoginSuccessFailures(t *testing.T) {
	for _, failure := range []string{"nil result", "nil profile", "wrong owner", "unsafe bytes", "invalid client", "expired access", "canceled", "invalid cookie"} {
		t.Run(failure, func(t *testing.T) {
			result, profile := loginSuccessFixture()
			clientType := "web"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "nil result":
				result = nil
			case "nil profile":
				profile = nil
			case "wrong owner":
				result.Session.UserID = 99
			case "unsafe bytes":
				profile.QuotaBytes = 1 << 53
			case "invalid client":
				clientType = "unknown"
			case "expired access":
				result.AccessToken.ExpiresAt = time.Now().Add(-time.Second)
			case "canceled":
				cancel()
			case "invalid cookie":
				result.RefreshToken = "fixture-refresh;\r\ninvalid"
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/test-login-success", func(requestContext *gin.Context) {
				handler.LoginSuccess(requestContext, result, profile, clientType, false)
			}, func(*gin.Context) { t.Error("交付失败后仍执行下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("POST", "/test-login-success", nil).WithContext(ctx))
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil ||
				response.Header().Get("Set-Cookie") != "" || strings.Contains(response.Body.String(), "fixture-") {
				t.Fatal("失败交付返回了凭证、Cookie 或异常正文")
			}
		})
	}
}

func TestLoginSuccessPreservesCommittedResponse(t *testing.T) {
	result, profile := loginSuccessFixture()
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-login-success", func(requestContext *gin.Context) {
		requestContext.String(202, "already-written")
		handler.LoginSuccess(requestContext, result, profile, "web", false)
	}, func(*gin.Context) { t.Error("已提交响应后仍继续处理") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/test-login-success", nil))
	if response.Code != 202 || response.Body.String() != "already-written" || response.Header().Get("Set-Cookie") != "" {
		t.Fatal("已提交响应被覆盖、追加或设置凭证 Cookie")
	}
}

func TestLoginSuccessCookieJar(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			result, profile := loginSuccessFixture()
			router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			policy := security.NewLoginSourcePolicy([]string{"http://127.0.0.1:5173"})
			router.POST("/api/v1/auth/test-login-success", func(requestContext *gin.Context) {
				if !policy.Allows(requestContext.Request, "web") {
					handler.ClientSourceForbidden(requestContext)
					return
				}
				handler.LoginSuccess(requestContext, result, profile, "web", secure)
			})
			router.POST("/api/v1/auth/test-native-success", func(requestContext *gin.Context) {
				if !policy.Allows(requestContext.Request, "native") {
					handler.ClientSourceForbidden(requestContext)
					return
				}
				handler.LoginSuccess(requestContext, result, profile, "native", secure)
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
			request, err := http.NewRequest("POST", server.URL+"/api/v1/auth/test-login-success", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set(security.ClientHeader, "web")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("真实登录交付请求失败")
			}
			body, readError := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readError != nil || response.StatusCode != 200 || strings.Contains(string(body), result.RefreshToken) {
				t.Fatal("Web 网络响应交付失败或泄漏刷新原文")
			}
			refreshURL, err := url.Parse(server.URL + "/api/v1/auth/refresh")
			if err != nil {
				t.Fatal(err)
			}
			cookies := jar.Cookies(refreshURL)
			if len(cookies) != 1 || cookies[0].Name != handler.RefreshCookieName || cookies[0].Value != result.RefreshToken {
				t.Fatal("真实客户端未保存原始刷新 Cookie")
			}
			for _, path := range []string{"/api/v1/me", "/api/v1/auth-other", "/"} {
				outside, err := url.Parse(server.URL + path)
				if err != nil {
					t.Fatal(err)
				}
				if len(jar.Cookies(outside)) != 0 {
					t.Error("刷新 Cookie 超出认证路径范围")
				}
			}
			otherHost := *refreshURL
			otherHost.Host = "other.example"
			if len(jar.Cookies(&otherHost)) != 0 {
				t.Error("host-only Cookie 被用于其他主机")
			}
			if secure {
				// 本机 Go 对回环地址有安全来源兼容，普通主机使用内存 URL 单独验证。
				remote, err := url.Parse("https://cloudnest.example/api/v1/auth/refresh")
				if err != nil {
					t.Fatal(err)
				}
				jar.SetCookies(remote, response.Cookies())
				if len(jar.Cookies(remote)) != 1 {
					t.Fatal("普通 HTTPS 主机未保存 Secure Cookie")
				}
				insecure := *remote
				insecure.Scheme = "http"
				if len(jar.Cookies(&insecure)) != 0 {
					t.Error("Secure Cookie 被用于 HTTP")
				}
			}
			nativeRequest, err := http.NewRequest("POST", server.URL+"/api/v1/auth/test-native-success", nil)
			if err != nil {
				t.Fatal(err)
			}
			nativeRequest.Header.Set(security.ClientHeader, "native")
			nativeResponse, err := client.Do(nativeRequest)
			if err != nil {
				t.Fatal("原生交付网络请求失败")
			}
			var nativeBody dto.LoginResponse
			decodeError := json.NewDecoder(nativeResponse.Body).Decode(&nativeBody)
			_ = nativeResponse.Body.Close()
			if decodeError != nil || nativeResponse.StatusCode != 200 || len(nativeResponse.Cookies()) != 0 || nativeBody.Data.RefreshToken != result.RefreshToken {
				t.Fatal("原生网络响应未按 JSON 模式交付")
			}
		})
	}
}
