package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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

func TestLogoutHandlerIntegration(t *testing.T) {
	// 1. 复用独占库生命周期，装配真实事务、JWT、授权及刷新服务。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
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
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	credentials := service.NewRefreshCredentialService(store.FindRefreshSession)
	logout := service.NewLogoutService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.LogoutTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	})
	rollbackLogout := service.NewLogoutService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.LogoutTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if err := action(txContext, txStore); err != nil {
				return err
			}
			return errors.New("private-failure-before-commit")
		})
	})
	refreshSessions := service.NewRefreshSessionService(credentials.Verify, func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
		return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
			return action(txContext, txStore, current)
		})
	})
	refreshTokens := service.NewRefreshTokenService(refreshSessions.Refresh, signer.Issue)
	origin := "http://127.0.0.1:5173"
	policy := security.NewLoginSourcePolicy([]string{origin})
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
	router.POST("/api/v1/auth/test-logout", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), handler.Logout(policy, logout.Logout, false))
	router.POST("/api/v1/auth/test-logout-rollback", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), handler.Logout(policy, rollbackLogout.Logout, false))
	router.POST("/api/v1/auth/test-refresh", handler.Refresh(policy, refreshTokens.Refresh, false))
	router.GET("/test-protected", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), func(requestContext *gin.Context) { requestContext.Status(204) })
	// 此测试钩子在认证完成后改变数据库状态，观察退出事务的锁内复查。
	router.POST("/api/v1/auth/test-logout-stale/:state", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		actor, exists := requestcontext.Actor(requestContext.Request.Context())
		if !exists {
			handler.InternalError(requestContext, nil)
			return
		}
		var statement string
		var identifier uint64
		switch requestContext.Param("state") {
		case "session":
			statement, identifier = "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.SessionID
		case "device":
			statement, identifier = "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.DeviceID
		case "account":
			statement, identifier = "UPDATE users SET status='DISABLED' WHERE id=?", actor.UserID
		default:
			t.Error("未知状态测试夹具")
			handler.InternalError(requestContext, nil)
			return
		}
		if _, err := pool.ExecContext(requestContext.Request.Context(), statement, identifier); err != nil {
			t.Error("测试状态写入失败", err)
			handler.InternalError(requestContext, nil)
		}
	}, handler.Logout(policy, logout.Logout, false))
	server := httptest.NewServer(router)
	defer server.Close()
	var secrets []string
	type fixture struct {
		session     domain.LoginSession
		access, raw string
	}
	createFixture := func(t *testing.T, installation, clientType string) fixture {
		t.Helper()
		platform := "WEB"
		if clientType == "native" {
			platform = "WINDOWS"
		}
		created, err := loginSessions.Create(ctx, domain.VerifiedAccount{ID: 1}, installation, "退出处理联测设备", platform)
		if err != nil || created == nil {
			t.Fatal("准备真实会话失败", err)
		}
		token, err := signer.Issue(created.Session)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, token.Token, created.RefreshToken)
		return fixture{session: created.Session, access: token.Token, raw: created.RefreshToken}
	}
	makeRequest := func(t *testing.T, method, path, body, access, clientType string) *http.Request {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+access)
		request.Header.Set(security.ClientHeader, clientType)
		if clientType == "web" {
			request.Header.Set("Origin", origin)
		}
		return request
	}
	perform := func(t *testing.T, client *http.Client, request *http.Request) (*http.Response, []byte) {
		t.Helper()
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("真实 HTTP 请求失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil {
			t.Fatal("读取 HTTP 响应失败", readError, closeError)
		}
		return response, encoded
	}
	client := server.Client()
	client.Timeout = 5 * time.Second
	// 2. 两种客户端通过真实认证完成退出，核对 Cookie 和当前会话范围。
	for _, clientType := range []string{"web", "native"} {
		t.Run(clientType, func(t *testing.T) {
			installation := uuid.NewString()
			current := createFixture(t, installation, clientType)
			another := createFixture(t, installation, clientType)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client.Jar = jar
			refreshURL, err := url.Parse(server.URL + "/api/v1/auth/test-refresh")
			if err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(refreshURL, []*http.Cookie{{Name: handler.RefreshCookieName, Value: current.raw, Path: handler.RefreshCookiePath}})
			input := `{"client_type":"` + clientType + `"}`
			forbidden := makeRequest(t, "POST", "/api/v1/auth/test-logout", input, current.access, clientType)
			forbidden.Header.Del(security.ClientHeader)
			rejected, _ := perform(t, client, forbidden)
			if rejected.StatusCode != 403 || len(rejected.Cookies()) != 0 {
				t.Fatal("来源检查未拒绝不合规请求")
			}
			if actor, err := authorization.Authorize(ctx, current.access); err != nil || actor == nil {
				t.Fatal("来源拒绝后当前会话异常", err)
			}
			response, encoded := perform(t, client, makeRequest(t, "POST", "/api/v1/auth/test-logout", input, current.access, clientType))
			var body dto.LogoutResponse
			if response.StatusCode != 200 || json.Unmarshal(encoded, &body) != nil || !body.Data.Revoked || body.RequestID == "" || body.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("真实退出成功交付错误")
			}
			cookies := jar.Cookies(refreshURL)
			if clientType == "web" {
				if len(cookies) != 0 || len(response.Cookies()) != 1 || response.Cookies()[0].MaxAge != -1 {
					t.Fatal("Web 退出未清理刷新 Cookie")
				}
			} else if len(response.Cookies()) != 0 || len(cookies) != 1 || cookies[0].Value != current.raw {
				t.Fatal("原生退出的 Web Cookie 边界异常")
			}
			protected, _ := perform(t, client, makeRequest(t, "GET", "/test-protected", "", current.access, clientType))
			if protected.StatusCode != 401 {
				t.Fatal("退出后的 JWT 仍通过数据库授权")
			}
			refreshInput := dto.RefreshRequest{ClientType: clientType}
			if clientType == "native" {
				refreshInput.RefreshToken = current.raw
			}
			refreshBody, err := json.Marshal(refreshInput)
			if err != nil {
				t.Fatal(err)
			}
			stale := makeRequest(t, "POST", "/api/v1/auth/test-refresh", string(refreshBody), current.access, clientType)
			if clientType == "web" {
				stale.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: current.raw})
			}
			refreshResponse, refreshEncoded := perform(t, client, stale)
			var refreshError dto.ErrorResponse
			if refreshResponse.StatusCode != 401 || json.Unmarshal(refreshEncoded, &refreshError) != nil || refreshError.Error.Code != "SESSION_REVOKED" {
				t.Fatal("重放已撤销会话的刷新原文未被拒绝")
			}
			replay, _ := perform(t, client, makeRequest(t, "POST", "/api/v1/auth/test-logout", input, current.access, clientType))
			if replay.StatusCode != 401 || len(replay.Cookies()) != 0 {
				t.Fatal("已撤销 JWT 重复退出的认证边界错误")
			}
			otherResponse, _ := perform(t, client, makeRequest(t, "GET", "/test-protected", "", another.access, clientType))
			verified, err := credentials.Verify(ctx, another.raw)
			if otherResponse.StatusCode != 204 || err != nil || verified == nil || verified.Session.ID != another.session.ID {
				t.Fatal("同设备另一会话的授权或刷新状态异常", err)
			}
		})
	}
	client.Jar = nil
	// 3. 回调故障使撤销回滚，HTTP 返回内部错误，原会话继续有效。
	t.Run("rollback before commit", func(t *testing.T) {
		current := createFixture(t, uuid.NewString(), "web")
		response, encoded := perform(t, client, makeRequest(t, "POST", "/api/v1/auth/test-logout-rollback", `{"client_type":"web"}`, current.access, "web"))
		var body dto.ErrorResponse
		if response.StatusCode != 500 || json.Unmarshal(encoded, &body) != nil || body.Error.Code != "INTERNAL_ERROR" || len(response.Cookies()) != 0 {
			t.Fatal("回滚故障的 HTTP 结果错误")
		}
		actor, accessError := authorization.Authorize(ctx, current.access)
		verified, refreshError := credentials.Verify(ctx, current.raw)
		if accessError != nil || refreshError != nil || actor == nil || verified == nil {
			t.Fatal("回滚后原会话异常", accessError, refreshError)
		}
	})
	// 4. 认证后发生撤销或禁用，用户事务再次检查并返回对应错误。
	for _, state := range []string{"session", "device", "account"} {
		t.Run("changed after authorization "+state, func(t *testing.T) {
			current := createFixture(t, uuid.NewString(), "web")
			if state == "account" {
				defer func() {
					if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
						t.Error("恢复测试账号失败", err)
					}
				}()
			}
			response, encoded := perform(t, client, makeRequest(t, "POST", "/api/v1/auth/test-logout-stale/"+state, `{"client_type":"web"}`, current.access, "web"))
			expectedStatus, expectedCode := 401, "SESSION_REVOKED"
			if state == "account" {
				expectedStatus, expectedCode = 403, "ACCOUNT_DISABLED"
			}
			var body dto.ErrorResponse
			if response.StatusCode != expectedStatus || json.Unmarshal(encoded, &body) != nil || body.Error.Code != expectedCode || len(response.Cookies()) != 0 {
				t.Fatal("认证后状态变化未被事务复查拒绝")
			}
		})
	}
	// 5. 完成日志沿用原文保护，独占数据库与连接由测试生命周期清理。
	for _, secret := range secrets {
		if secret == "" || strings.Contains(logs.String(), secret) {
			t.Fatal("令牌原文为空或进入日志")
		}
	}
	if strings.Contains(logs.String(), "private-") {
		t.Fatal("完成日志包含内部错误")
	}
}
