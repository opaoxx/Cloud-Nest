package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"golang.org/x/crypto/bcrypt"
)

func TestLoginHandlerRealComponents(t *testing.T) {
	// 1. 使用测试摘要连接真实凭证、共享限流及 JWT 组件，资料和会话由内存提供。
	hash, err := bcrypt.GenerateFromPassword([]byte("root"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	credentials := service.NewCredentialService(func(_ context.Context, username string) (*domain.LoginAccount, error) {
		if username != "root" {
			return nil, domain.ErrAccountNotFound
		}
		return &domain.LoginAccount{ID: 7, PasswordHash: string(hash), Status: "ACTIVE"}, nil
	})
	limiter := service.NewLoginFailureLimiter()
	gate := security.NewLoginAttemptGate(limiter.Begin, credentials.Authenticate)
	cfg := config.JWTConfig{SigningKey: bytes.Repeat([]byte{7}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	profiles := service.NewUserProfileService(func(context.Context, uint64) (*domain.UserProfile, error) {
		return &domain.UserProfile{ID: 7, Username: "root", DisplayName: "学习用户", Status: "ACTIVE", RootEntryID: 17, QuotaBytes: 100}, nil
	})
	create := func(_ context.Context, account domain.VerifiedAccount, installationID, name, platform string) (*domain.LoginTokenResult, error) {
		session := domain.LoginSession{ID: 11, UserID: account.ID, DeviceID: 13, ExpiresAt: time.Now().UTC().Add(time.Hour)}
		token, err := signer.Issue(session)
		if err != nil {
			return nil, err
		}
		refresh, _ := service.GenerateRefreshToken()
		return &domain.LoginTokenResult{Device: domain.LoginDevice{ID: 13, UserID: account.ID, InstallationID: installationID, Name: name, Platform: platform}, Session: session, AccessToken: *token, RefreshToken: refresh}, nil
	}
	var logs bytes.Buffer
	router := NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	policy := security.NewLoginSourcePolicy([]string{"http://127.0.0.1:5173"})
	router.POST("/test-login", handler.Login(policy, gate.Authenticate, create, profiles.Get, false))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	var deliveredSecrets []string
	// 2. 真实 TCP 中 Web/原生按各自来源通过，返回的 JWT 可被已有校验器验证。
	for _, clientType := range []string{"web", "native"} {
		t.Run(clientType, func(t *testing.T) {
			request := loginHandlerRequest(t, "/test-login", clientType, "root")
			request.URL.Scheme, request.URL.Host, request.RequestURI = "http", strings.TrimPrefix(server.URL, "http://"), ""
			response, err := client.Do(request)
			if err != nil {
				t.Fatal("登录处理函数网络请求失败")
			}
			body, readError := io.ReadAll(response.Body)
			closeError := response.Body.Close()
			var payload dto.LoginResponse
			if readError != nil || closeError != nil || response.StatusCode != 200 || json.Unmarshal(body, &payload) != nil || payload.Data.User.ID != "7" || payload.Data.Session.DeviceID != "13" || payload.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("真实组件返回的登录结果错误")
			}
			identity, err := verifier.Verify(payload.Data.AccessToken)
			if err != nil || identity == nil || identity.UserID != 7 || identity.SessionID != 11 {
				t.Fatal("登录处理函数返回的 JWT 不能用于后续认证")
			}
			deliveredSecrets = append(deliveredSecrets, payload.Data.AccessToken)
			if clientType == "web" {
				cookies := response.Cookies()
				if len(cookies) != 1 || cookies[0].Name != handler.RefreshCookieName || payload.Data.RefreshToken != "" {
					t.Fatal("Web 刷新凭证交付错误")
				}
				deliveredSecrets = append(deliveredSecrets, cookies[0].Value)
			} else {
				if len(response.Cookies()) != 0 || payload.Data.RefreshToken == "" {
					t.Fatal("原生刷新凭证交付错误")
				}
				deliveredSecrets = append(deliveredSecrets, payload.Data.RefreshToken)
			}
		})
	}
	server.Close()
	for _, secret := range deliveredSecrets {
		if strings.Contains(logs.String(), secret) {
			t.Error("真实登录组件日志泄漏凭证")
		}
	}
}

func TestLoginHandlerSharedFailureLimit(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("root"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	credentialCalls := 0
	credentials := service.NewCredentialService(func(context.Context, string) (*domain.LoginAccount, error) {
		credentialCalls++
		return &domain.LoginAccount{ID: 7, PasswordHash: string(hash), Status: "ACTIVE"}, nil
	})
	limiter := service.NewLoginFailureLimiter()
	gate := security.NewLoginAttemptGate(limiter.Begin, credentials.Authenticate)
	policy := security.NewLoginSourcePolicy([]string{"http://127.0.0.1:5173"})
	router := NewRouter(logging.New(io.Discard), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	create := func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginTokenResult, error) {
		t.Error("错误凭证调用了会话创建")
		return nil, nil
	}
	profile := func(context.Context, uint64) (*domain.UserProfile, error) {
		t.Error("错误凭证调用了资料查询")
		return nil, nil
	}
	router.POST("/test-login", handler.Login(policy, gate.Authenticate, create, profile, false))
	for attempt := 1; attempt <= 11; attempt++ {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, loginHandlerRequest(t, "/test-login", "web", "wrong-password"))
		expected := 401
		if attempt == 11 {
			expected = 429
		}
		if response.Code != expected {
			t.Fatalf("共享失败额度未生效 attempt=%d status=%d", attempt, response.Code)
		}
	}
	if credentialCalls != 10 {
		t.Fatal("限流拒绝后仍查询了凭证")
	}
}
