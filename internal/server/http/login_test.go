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

func loginHandlerRequest(t *testing.T, path, clientType, password string) *http.Request {
	t.Helper()
	platform := "WEB"
	if clientType == "native" {
		platform = "ANDROID"
	}
	input := dto.LoginRequest{Username: "root", Password: password, InstallationID: "28ee758a-0727-42de-ae7d-3bb33155e148", DeviceName: "请求设备名", Platform: platform, ClientType: clientType}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(security.ClientHeader, clientType)
	if clientType == "web" {
		request.Header.Set("Origin", "http://127.0.0.1:5173")
	}
	request.RemoteAddr = "192.0.2.1:1234"
	return request
}

func TestLoginHandlerFlow(t *testing.T) {
	for _, testCase := range []struct {
		name, steps, code string
		status            int
	}{
		{name: "web success", steps: "authenticate,profile,tokens", status: 200},
		{name: "native success", steps: "authenticate,profile,tokens", status: 200},
		{name: "bad json", status: 400, code: "INVALID_ARGUMENT"},
		{name: "bad fields", status: 400, code: "INVALID_ARGUMENT"},
		{name: "large body", status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "bad content type", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "bad source", status: 403, code: "CLIENT_SOURCE_FORBIDDEN"},
		{name: "cors refusal", status: 403, code: "CORS_FORBIDDEN"},
		{name: "preflight", status: 204},
		{name: "bad credentials", steps: "authenticate", status: 401, code: "AUTH_REQUIRED"},
		{name: "wrapped bad credentials", steps: "authenticate", status: 401, code: "AUTH_REQUIRED"},
		{name: "disabled credentials", steps: "authenticate", status: 403, code: "ACCOUNT_DISABLED"},
		{name: "credential fault", steps: "authenticate", status: 500, code: "INTERNAL_ERROR"},
		{name: "limited", steps: "authenticate", status: 429, code: "RATE_LIMITED"},
		{name: "limited with account", steps: "authenticate", status: 500, code: "INTERNAL_ERROR"},
		{name: "negative wait", steps: "authenticate", status: 500, code: "INTERNAL_ERROR"},
		{name: "nil account", steps: "authenticate", status: 500, code: "INTERNAL_ERROR"},
		{name: "zero account", steps: "authenticate", status: 500, code: "INTERNAL_ERROR"},
		{name: "profile missing", steps: "authenticate,profile", status: 401, code: "AUTH_REQUIRED"},
		{name: "profile disabled", steps: "authenticate,profile", status: 403, code: "ACCOUNT_DISABLED"},
		{name: "profile fault", steps: "authenticate,profile", status: 500, code: "INTERNAL_ERROR"},
		{name: "nil profile", steps: "authenticate,profile", status: 500, code: "INTERNAL_ERROR"},
		{name: "wrong profile", steps: "authenticate,profile", status: 500, code: "INTERNAL_ERROR"},
		{name: "unsafe profile", steps: "authenticate,profile", status: 500, code: "INTERNAL_ERROR"},
		{name: "token fault", steps: "authenticate,profile,tokens", status: 500, code: "INTERNAL_ERROR"},
		{name: "token disabled", steps: "authenticate,profile,tokens", status: 403, code: "ACCOUNT_DISABLED"},
		{name: "nil tokens", steps: "authenticate,profile,tokens", status: 500, code: "INTERNAL_ERROR"},
		{name: "wrong token owner", steps: "authenticate,profile,tokens", status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel before", status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel credentials", steps: "authenticate", status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel profile", steps: "authenticate,profile", status: 500, code: "INTERNAL_ERROR"},
		{name: "cancel tokens", steps: "authenticate,profile,tokens", status: 500, code: "INTERNAL_ERROR"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.name == "cancel before" {
				cancel()
			}
			var steps []string
			result, profile := loginSuccessFixture()
			var logs bytes.Buffer
			clientType := "web"
			if testCase.name == "native success" {
				clientType = "native"
			}
			policy := security.NewLoginSourcePolicy([]string{"http://127.0.0.1:5173"})
			authenticate := func(request *http.Request, username, password string) (*domain.VerifiedAccount, time.Duration, error) {
				steps = append(steps, "authenticate")
				if username != "root" || password != " private-password " || request.Context().Done() != ctx.Done() {
					t.Error("凭证或请求上下文未按原文传递")
				}
				switch testCase.name {
				case "bad credentials":
					return nil, 0, domain.ErrInvalidCredentials
				case "wrapped bad credentials":
					return nil, 0, fmt.Errorf("fixture wrap: %w", domain.ErrInvalidCredentials)
				case "disabled credentials":
					return nil, 0, domain.ErrAccountDisabled
				case "credential fault":
					return nil, 0, errors.New("fixture-credential-secret")
				case "limited":
					return nil, 1500 * time.Millisecond, nil
				case "limited with account":
					return &domain.VerifiedAccount{ID: 7}, time.Second, nil
				case "negative wait":
					return nil, -time.Second, nil
				case "nil account":
					return nil, 0, nil
				case "zero account":
					return &domain.VerifiedAccount{}, 0, nil
				case "cancel credentials":
					cancel()
				}
				return &domain.VerifiedAccount{ID: 7}, 0, nil
			}
			getProfile := func(received context.Context, userID uint64) (*domain.UserProfile, error) {
				steps = append(steps, "profile")
				if userID != 7 || received.Done() != ctx.Done() {
					t.Error("资料查询未使用已验证用户和原上下文")
				}
				switch testCase.name {
				case "profile missing":
					return nil, domain.ErrAccountNotFound
				case "profile disabled":
					return nil, domain.ErrAccountDisabled
				case "profile fault":
					return nil, errors.New("fixture-profile-secret")
				case "nil profile":
					return nil, nil
				case "wrong profile":
					profile.ID = 99
				case "unsafe profile":
					profile.QuotaBytes = 1 << 53
				case "cancel profile":
					cancel()
				}
				return profile, nil
			}
			create := func(received context.Context, account domain.VerifiedAccount, installationID, name, platform string) (*domain.LoginTokenResult, error) {
				steps = append(steps, "tokens")
				expectedPlatform := "WEB"
				if clientType == "native" {
					expectedPlatform = "ANDROID"
				}
				if received.Done() != ctx.Done() || account.ID != 7 || installationID != "28ee758a-0727-42de-ae7d-3bb33155e148" || name != "请求设备名" || platform != expectedPlatform {
					t.Error("会话创建参数错误")
				}
				switch testCase.name {
				case "token fault":
					return nil, errors.New("fixture-token-secret")
				case "token disabled":
					return nil, domain.ErrAccountDisabled
				case "nil tokens":
					return nil, nil
				case "wrong token owner":
					result.Session.UserID = 99
				case "cancel tokens":
					cancel()
				}
				return result, nil
			}
			router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
			router.POST("/test-login", handler.Login(policy, authenticate, create, getProfile, false), func(*gin.Context) { t.Error("登录响应后继续执行下游") })
			request := loginHandlerRequest(t, "/test-login?token=fixture-query-secret", clientType, " private-password ").WithContext(ctx)
			switch testCase.name {
			case "bad json":
				request.Body = io.NopCloser(strings.NewReader(`{"password":`))
			case "bad fields":
				request.Body = io.NopCloser(strings.NewReader(`{}`))
			case "large body":
				request.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", int(binding.MaxJSONBodyBytes)+1)))
			case "bad content type":
				request.Header.Set("Content-Type", "text/plain")
			case "bad source":
				request.Header.Del("Origin")
			case "cors refusal":
				request.Header.Set("Origin", "https://unknown.example")
			case "preflight":
				request.Method = "OPTIONS"
				request.Header.Set("Access-Control-Request-Method", "POST")
				request.Header.Set("Access-Control-Request-Headers", "content-type,x-cloudnest-client")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || strings.Join(steps, ",") != testCase.steps {
				t.Fatalf("状态或阶段顺序错误 status=%d steps=%s", response.Code, strings.Join(steps, ","))
			}
			if testCase.status == 204 {
				return
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("登录响应未禁止缓存")
			}
			if testCase.status == 200 {
				var body dto.LoginResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Data.User.ID != "7" || body.Data.AccessToken != result.AccessToken.Token || body.RequestID != response.Header().Get("X-Request-ID") {
					t.Fatal("成功登录正文不一致")
				}
				if clientType == "native" {
					if body.Data.RefreshToken != result.RefreshToken || response.Header().Get("Set-Cookie") != "" {
						t.Fatal("原生模式交付错误")
					}
				} else if body.Data.RefreshToken != "" || len(response.Result().Cookies()) != 1 {
					t.Fatal("Web 模式交付错误")
				}
			} else {
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != testCase.code || body.RequestID != response.Header().Get("X-Request-ID") || response.Header().Get("Set-Cookie") != "" {
					t.Fatal("登录错误响应或凭证发布行为错误")
				}
				if strings.Contains(response.Body.String(), "fixture-") {
					t.Fatal("错误响应回显了敏感内容")
				}
				if testCase.code == "AUTH_REQUIRED" && strings.Contains(testCase.name, "bad credentials") && body.Error.Message != "用户名或密码错误" {
					t.Fatal("凭证错误未使用统一提示")
				}
			}
			if testCase.status == 429 && response.Header().Get("Retry-After") != "2" {
				t.Fatal("限流等待时间错误")
			}
			if strings.Contains(logs.String(), "fixture-") || strings.Contains(logs.String(), result.AccessToken.Token) || strings.Contains(logs.String(), result.RefreshToken) || strings.Contains(logs.String(), "private-password") {
				t.Fatal("登录日志泄漏凭证或请求内容")
			}
		})
	}
}

func TestLoginHandlerDependencies(t *testing.T) {
	for _, missing := range []string{"policy", "authenticate", "tokens", "profile"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少登录依赖未暴露装配错误")
				}
			}()
			policy := security.NewLoginSourcePolicy(nil)
			authenticate := func(*http.Request, string, string) (*domain.VerifiedAccount, time.Duration, error) {
				return nil, 0, nil
			}
			create := func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginTokenResult, error) {
				return nil, nil
			}
			profile := func(context.Context, uint64) (*domain.UserProfile, error) { return nil, nil }
			switch missing {
			case "policy":
				policy = nil
			case "authenticate":
				authenticate = nil
			case "tokens":
				create = nil
			case "profile":
				profile = nil
			}
			handler.Login(policy, authenticate, create, profile, false)
		})
	}
}
