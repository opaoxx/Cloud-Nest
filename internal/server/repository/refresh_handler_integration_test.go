package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRefreshHandlerIntegration(t *testing.T) {
	// 1. 使用既有独占库和正式设备/会话表，装配真实服务与签发器。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 30 * 24 * time.Hour}
	loginSessions, err := service.NewLoginSessionService(configuration, func(ctx context.Context, userID uint64, action func(context.Context, service.LoginSessionTransaction) error) error {
		return store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	credentials := service.NewRefreshCredentialService(store.FindRefreshSession)
	refreshSessions := service.NewRefreshSessionService(credentials.Verify, func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
		return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
			return action(txContext, txStore, current)
		})
	})
	refreshTokens := service.NewRefreshTokenService(refreshSessions.Refresh, signer.Issue)
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	origin := "http://127.0.0.1:5173"
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
	router.POST("/api/v1/auth/test-refresh", handler.Refresh(security.NewLoginSourcePolicy([]string{origin}), refreshTokens.Refresh, false))
	router.GET("/test-protected", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		actor, exists := requestcontext.Actor(requestContext.Request.Context())
		if !exists {
			handler.InternalError(requestContext, nil)
			return
		}
		requestContext.Header("X-Test-Session", strconv.FormatUint(actor.SessionID, 10))
		requestContext.Status(204)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	postRefresh := func(t *testing.T, clientType, raw string) (*http.Response, []byte) {
		t.Helper()
		input := dto.RefreshRequest{ClientType: clientType}
		if clientType == "native" {
			input.RefreshToken = raw
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/v1/auth/test-refresh", bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(security.ClientHeader, clientType)
		if clientType == "web" {
			request.Header.Set("Origin", origin)
			request.AddCookie(&http.Cookie{Name: security.RefreshCookieName, Value: raw})
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("真实刷新处理请求失败", err)
		}
		body, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil {
			t.Fatal("读取刷新响应失败", readError, closeError)
		}
		return response, body
	}
	var secrets []string
	// 2. 两种客户端各连续刷新两次，核对 Cookie/JSON、摘要和数据库授权。
	for _, clientType := range []string{"web", "native"} {
		t.Run(clientType, func(t *testing.T) {
			platform := "WEB"
			if clientType == "native" {
				platform = "WINDOWS"
			}
			created, err := loginSessions.Create(ctx, domain.VerifiedAccount{ID: 1}, uuid.NewString(), "刷新处理联测设备", platform)
			if err != nil || created == nil {
				t.Fatal("准备真实会话失败", err)
			}
			currentRaw := created.RefreshToken
			secrets = append(secrets, currentRaw)
			for generation := uint64(2); generation <= 3; generation++ {
				oldRaw := currentRaw
				response, body := postRefresh(t, clientType, oldRaw)
				var payload dto.RefreshResponse
				if response.StatusCode != 200 || json.Unmarshal(body, &payload) != nil || payload.RequestID == "" || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" {
					t.Fatal("完整刷新响应错误", response.StatusCode)
				}
				if clientType == "web" {
					cookies := response.Cookies()
					if len(cookies) != 1 || cookies[0].Name != security.RefreshCookieName || !cookies[0].HttpOnly || payload.Data.RefreshToken != "" || strings.Contains(string(body), cookies[0].Value) {
						t.Fatal("Web 完整交付错误")
					}
					currentRaw = cookies[0].Value
				} else {
					if len(response.Cookies()) != 0 || payload.Data.RefreshToken == "" {
						t.Fatal("原生完整交付错误")
					}
					currentRaw = payload.Data.RefreshToken
				}
				secrets = append(secrets, currentRaw, payload.Data.AccessToken)
				current, err := credentials.Verify(ctx, currentRaw)
				if err != nil || current == nil || current.TokenGeneration != generation || current.Session != created.Session || current.TokenHash != sha256.Sum256([]byte(currentRaw)) {
					t.Fatal("HTTP 结果未对应已提交数据库轮换", err)
				}
				protected, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/test-protected", nil)
				if err != nil {
					t.Fatal(err)
				}
				protected.Header.Set("Authorization", "Bearer "+payload.Data.AccessToken)
				protectedResponse, err := client.Do(protected)
				if err != nil {
					t.Fatal("访问刷新后的受保护测试端点失败", err)
				}
				_, readError := io.Copy(io.Discard, protectedResponse.Body)
				_ = protectedResponse.Body.Close()
				if readError != nil || protectedResponse.StatusCode != 204 || protectedResponse.Header.Get("X-Test-Session") != strconv.FormatUint(created.Session.ID, 10) {
					t.Fatal("新 JWT 的实际会话授权失败", readError)
				}
				staleResponse, staleBody := postRefresh(t, clientType, oldRaw)
				var rejected dto.ErrorResponse
				if staleResponse.StatusCode != 401 || json.Unmarshal(staleBody, &rejected) != nil || rejected.Error.Code != "AUTH_REQUIRED" || len(staleResponse.Cookies()) != 0 {
					t.Fatal("旧原文未在完整处理链中被拒绝")
				}
			}
		})
	}
	// 3. 核对完成日志中的原文保护，所有资源由测试生命周期清理。
	for _, secret := range secrets {
		if secret == "" || strings.Contains(logs.String(), secret) {
			t.Fatal("原文为空或进入完成日志")
		}
	}
}
