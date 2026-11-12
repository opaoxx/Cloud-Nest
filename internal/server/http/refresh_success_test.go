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
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func refreshSuccessFixture() *domain.RefreshTokenResult {
	result, _ := loginSuccessFixture()
	return &domain.RefreshTokenResult{Session: result.Session, TokenGeneration: 2, AccessToken: result.AccessToken, RefreshToken: result.RefreshToken}
}

func TestRefreshSuccessDelivery(t *testing.T) {
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
			result := refreshSuccessFixture()
			before := *result
			result.Session.ExpiresAt = result.Session.ExpiresAt.In(time.FixedZone("UTC+8", 8*60*60))
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			var downstream int
			router.POST("/test-refresh-success", func(requestContext *gin.Context) {
				if testCase.pending {
					requestContext.Header("Content-Length", "3")
					requestContext.Header("Content-Encoding", "gzip")
					requestContext.Header("Content-Type", "application/octet-stream")
				}
				handler.RefreshSuccess(requestContext, result, testCase.clientType, testCase.secure)
			}, func(*gin.Context) { downstream++ })
			request := httptest.NewRequest("POST", "/test-refresh-success?token=fixture-query-secret", nil)
			request.Header.Set("Origin", "http://127.0.0.1:5173")
			request.Header.Set("X-Forwarded-Proto", "https")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != 200 || downstream != 0 || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" ||
				response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("Content-Encoding") != "" {
				t.Fatal("成功状态、响应头或中止行为错误")
			}
			var body dto.RefreshResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Data.AccessToken != result.AccessToken.Token || body.Data.ExpiresIn < 0 || body.Data.ExpiresIn > 900 ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") {
				t.Fatal("刷新正文或请求 ID 错误", err)
			}
			cookies := response.Result().Cookies()
			if testCase.clientType == "web" {
				if len(cookies) != 1 {
					t.Fatal("Web 响应未设置单个 Cookie")
				}
				cookie := cookies[0]
				if cookie.Name != handler.RefreshCookieName || cookie.Value != result.RefreshToken || cookie.Path != handler.RefreshCookiePath || cookie.Domain != "" || !cookie.HttpOnly ||
					cookie.SameSite != http.SameSiteStrictMode || cookie.Secure != testCase.secure || cookie.MaxAge != 0 || !cookie.Expires.Equal(result.Session.ExpiresAt.UTC().Truncate(time.Second)) ||
					body.Data.RefreshToken != "" || strings.Contains(response.Body.String(), result.RefreshToken) {
					t.Fatal("Web 刷新 Cookie 属性或 JSON 字段错误")
				}
			} else if len(cookies) != 0 || body.Data.RefreshToken != result.RefreshToken {
				t.Fatal("原生刷新交付错误")
			}
			if strings.Contains(logs.String(), result.RefreshToken) || strings.Contains(logs.String(), result.AccessToken.Token) || strings.Contains(logs.String(), "fixture-query-secret") {
				t.Fatal("完成日志包含凭证或查询参数")
			}
			if result.Session.ID != before.Session.ID || result.RefreshToken != before.RefreshToken || result.TokenGeneration != before.TokenGeneration {
				t.Fatal("交付改动了内部结果")
			}
		})
	}
}

// cancelBeforeRefreshWriteContext 在第二次 Err 检查时取消，覆盖编码后、写出前的边界。
type cancelBeforeRefreshWriteContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (ctx *cancelBeforeRefreshWriteContext) Err() error {
	ctx.checks++
	if ctx.checks == 2 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestRefreshSuccessFailures(t *testing.T) {
	for _, failure := range []string{"nil result", "zero owner", "unrotated generation", "invalid client", "expired session", "expired access", "canceled", "cancel before write", "invalid cookie"} {
		t.Run(failure, func(t *testing.T) {
			result := refreshSuccessFixture()
			clientType := "web"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch failure {
			case "nil result":
				result = nil
			case "zero owner":
				result.Session.UserID = 0
			case "unrotated generation":
				result.TokenGeneration = 1
			case "invalid client":
				clientType = "other"
			case "expired session":
				result.Session.ExpiresAt, result.AccessToken.ExpiresAt = time.Now().Add(-time.Second), time.Now().Add(-2*time.Second)
			case "expired access":
				result.AccessToken.ExpiresAt = time.Now().Add(-time.Second)
			case "canceled":
				cancel()
			case "invalid cookie":
				result.RefreshToken = "fixture-refresh;\r\ninvalid"
			}
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.POST("/test-refresh-success", func(requestContext *gin.Context) {
				if failure == "cancel before write" {
					lateContext, cancelLate := context.WithCancel(requestContext.Request.Context())
					defer cancelLate()
					requestContext.Request = requestContext.Request.WithContext(&cancelBeforeRefreshWriteContext{Context: lateContext, cancel: cancelLate})
				}
				handler.RefreshSuccess(requestContext, result, clientType, false)
			}, func(*gin.Context) { t.Error("失败交付后进入下游") })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest("POST", "/test-refresh-success", nil).WithContext(ctx))
			var body dto.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil ||
				body.RequestID == "" || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Set-Cookie") != "" || response.Header().Get("Cache-Control") != "no-store" ||
				response.Header().Get("Pragma") != "no-cache" || strings.Contains(response.Body.String(), "fixture-") {
				t.Fatal("失败交付的错误结构、请求 ID、Cookie 或凭证处理异常", err)
			}
		})
	}
}

func TestRefreshSuccessCommittedResponse(t *testing.T) {
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST("/test-refresh-success", func(requestContext *gin.Context) {
		requestContext.Header("Content-Type", "text/plain; charset=utf-8")
		requestContext.String(202, "already-written")
		handler.RefreshSuccess(requestContext, refreshSuccessFixture(), "web", false)
	}, func(*gin.Context) { t.Error("响应已提交后进入下游") })
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/test-refresh-success", nil))
	if response.Code != 202 || response.Body.String() != "already-written" || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" || response.Header().Get("Set-Cookie") != "" {
		t.Fatal("已提交响应被覆盖或追加凭证")
	}
}

func TestRefreshSuccessCookieJarAndJWT(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			result := refreshSuccessFixture()
			configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{7}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
			signer, err := service.NewAccessTokenSigner(configuration)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := service.NewAccessTokenVerifier(configuration)
			if err != nil {
				t.Fatal(err)
			}
			issued, err := signer.Issue(result.Session)
			if err != nil {
				t.Fatal(err)
			}
			result.AccessToken = *issued
			origin := "http://127.0.0.1:5173"
			router := NewRouter(logging.New(io.Discard), []string{origin}, func(context.Context) bool { return true })
			policy := security.NewLoginSourcePolicy([]string{origin})
			router.POST("/api/v1/auth/test-refresh-success", func(requestContext *gin.Context) {
				if !policy.Allows(requestContext.Request, "web") {
					handler.ClientSourceForbidden(requestContext)
					return
				}
				handler.RefreshSuccess(requestContext, result, "web", secure)
			})
			router.POST("/api/v1/auth/test-native-refresh-success", func(requestContext *gin.Context) {
				if !policy.Allows(requestContext.Request, "native") {
					handler.ClientSourceForbidden(requestContext)
					return
				}
				handler.RefreshSuccess(requestContext, result, "native", secure)
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
			// 同名、同路径旧 Cookie 在收到本次 Set-Cookie 后被替换。
			jar.SetCookies(refreshURL, []*http.Cookie{{Name: handler.RefreshCookieName, Value: "old-cookie", Path: handler.RefreshCookiePath}})
			request, err := http.NewRequest("POST", server.URL+"/api/v1/auth/test-refresh-success", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Origin", origin)
			request.Header.Set(security.ClientHeader, "web")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("真实刷新交付请求失败", err)
			}
			encoded, readError := io.ReadAll(response.Body)
			_ = response.Body.Close()
			var body dto.RefreshResponse
			if readError != nil || response.StatusCode != 200 || json.Unmarshal(encoded, &body) != nil || strings.Contains(string(encoded), result.RefreshToken) {
				t.Fatal("Web 网络响应异常")
			}
			identity, err := verifier.Verify(body.Data.AccessToken)
			if err != nil || identity == nil || identity.UserID != result.Session.UserID || identity.SessionID != result.Session.ID {
				t.Fatal("HTTP 交付 JWT 校验失败", err)
			}
			cookies := jar.Cookies(refreshURL)
			if len(cookies) != 1 || cookies[0].Value != result.RefreshToken {
				t.Fatal("Cookie Jar 未替换为新刷新原文")
			}
			for _, path := range []string{"/api/v1/me", "/api/v1/auth-other", "/"} {
				outside, err := url.Parse(server.URL + path)
				if err != nil {
					t.Fatal(err)
				}
				if len(jar.Cookies(outside)) != 0 {
					t.Error("Cookie 超出认证路径")
				}
			}
			otherHost := *refreshURL
			otherHost.Host = "other.example"
			if len(jar.Cookies(&otherHost)) != 0 {
				t.Error("Cookie 被用于其他主机")
			}
			if secure {
				// Go 的 Cookie Jar 对回环地址有安全来源兼容，普通主机使用内存 URL 验证。
				remote, err := url.Parse("https://cloudnest.example/api/v1/auth/refresh")
				if err != nil {
					t.Fatal(err)
				}
				jar.SetCookies(remote, response.Cookies())
				if len(jar.Cookies(remote)) != 1 {
					t.Fatal("普通 HTTPS URL 未保存 Cookie")
				}
				insecure := *remote
				insecure.Scheme = "http"
				if len(jar.Cookies(&insecure)) != 0 {
					t.Error("Secure Cookie 被用于普通 HTTP URL")
				}
			}
			nativeRequest, err := http.NewRequest("POST", server.URL+"/api/v1/auth/test-native-refresh-success", nil)
			if err != nil {
				t.Fatal(err)
			}
			nativeRequest.Header.Set(security.ClientHeader, "native")
			nativeResponse, err := client.Do(nativeRequest)
			if err != nil {
				t.Fatal("原生网络交付请求失败", err)
			}
			var native dto.RefreshResponse
			decodeError := json.NewDecoder(nativeResponse.Body).Decode(&native)
			_ = nativeResponse.Body.Close()
			if decodeError != nil || nativeResponse.StatusCode != 200 || len(nativeResponse.Cookies()) != 0 || native.Data.RefreshToken != result.RefreshToken {
				t.Fatal("原生网络交付错误", decodeError)
			}
		})
	}
}
