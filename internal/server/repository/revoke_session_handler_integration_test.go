package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRevokeSessionHandlerIntegration(t *testing.T) {
	// 1. 复用独占库，装配真实认证、撤销服务、事务及 HTTP 处理链。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
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
	withTransaction := func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RevokeSessionTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	}
	revoke := service.NewRevokeSessionService(withTransaction)
	failure := errors.New("private-before-commit")
	failing := service.NewRevokeSessionService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RevokeSessionTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if err := action(txContext, txStore); err != nil {
				return err
			}
			return failure
		})
	})
	type fixture struct {
		session         domain.LoginSession
		access, refresh string
	}
	var secrets []string
	newFixture := func(t *testing.T, userID uint64, installation string) fixture {
		t.Helper()
		refresh, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installation, "撤销处理函数测试", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		})
		if err != nil || session == nil {
			t.Fatal("创建会话夹具失败", err)
		}
		token, err := signer.Issue(*session)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, token.Token, refresh)
		return fixture{session: *session, access: token.Token, refresh: refresh}
	}
	assertState := func(t *testing.T, current fixture, revoked bool) {
		t.Helper()
		actor, accessError := authorization.Authorize(ctx, current.access)
		verified, refreshError := credentials.Verify(ctx, current.refresh)
		if revoked {
			if actor != nil || verified != nil || !errors.Is(accessError, domain.ErrSessionRevoked) || !errors.Is(refreshError, domain.ErrSessionRevoked) {
				t.Fatal("已撤销会话凭证仍有效", accessError, refreshError)
			}
		} else if accessError != nil || refreshError != nil || actor == nil || verified == nil || actor.SessionID != current.session.ID || verified.Session.ID != current.session.ID {
			t.Fatal("独立会话凭证异常", accessError, refreshError)
		}
	}
	readTimes := func(t *testing.T, sessionID uint64) (sql.NullTime, time.Time) {
		t.Helper()
		var revokedAt sql.NullTime
		var updatedAt time.Time
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at,updated_at FROM auth_sessions WHERE id=?", sessionID).Scan(&revokedAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		return revokedAt, updatedAt
	}
	type testModeKey struct{}
	var calls atomic.Int32
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.DELETE("/api/v1/test-revoke-sessions/:id", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure),
		func(requestContext *gin.Context) {
			requestContext.Request = requestContext.Request.WithContext(context.WithValue(requestContext.Request.Context(), testModeKey{}, requestContext.GetHeader("X-Test-Mode")))
		}, handler.RevokeSession(func(ctx context.Context, actor domain.Actor, targetID uint64) (*domain.RevokeSessionResult, error) {
			calls.Add(1)
			// 测试模式在认证后改变本测试库状态，验证事务入口的当前读复查。
			switch ctx.Value(testModeKey{}) {
			case "rollback":
				return failing.Revoke(ctx, actor, targetID)
			case "revoke caller":
				err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return txStore.RevokeCurrentSession(txContext) })
				if err != nil {
					return nil, err
				}
			case "remove caller":
				if _, err := pool.ExecContext(ctx, "DELETE FROM auth_sessions WHERE user_id=? AND id=?", actor.UserID, actor.SessionID); err != nil {
					return nil, err
				}
			case "expire caller":
				if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE user_id=? AND id=?", actor.UserID, actor.SessionID); err != nil {
					return nil, err
				}
			case "disable account":
				if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=?", actor.UserID); err != nil {
					return nil, err
				}
			}
			return revoke.Revoke(ctx, actor, targetID)
		}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	request := func(t *testing.T, target, token, mode string, status, serviceCalls int, code, field string) {
		t.Helper()
		before := calls.Load()
		message, err := http.NewRequestWithContext(ctx, "DELETE", server.URL+"/api/v1/test-revoke-sessions/"+target+"?user_id=99&id=99&note=private-query", strings.NewReader("{\"user_id\":\"99\",\"id\":\"99\"}"))
		if err != nil {
			t.Fatal(err)
		}
		message.Header.Set("Origin", "http://127.0.0.1:5173")
		message.Header.Set("X-Test-Mode", mode)
		message.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-cookie"})
		if token != "" {
			message.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(message)
		if err != nil {
			t.Fatal("撤销 HTTP 请求失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || int(calls.Load()-before) != serviceCalls ||
			response.Header.Get("X-Request-ID") == "" || response.Header.Get("Cache-Control") != "no-store" || len(response.Cookies()) != 0 {
			t.Fatal("网络状态、服务调用或响应头错误", response.StatusCode, calls.Load()-before, readError, closeError)
		}
		if status == 200 {
			var body dto.RevokeSessionResponse
			if json.Unmarshal(encoded, &body) != nil || !body.Data.Revoked || body.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" {
				t.Fatal("成功 JSON 或缓存头错误")
			}
			return
		}
		var body dto.ErrorResponse
		if json.Unmarshal(encoded, &body) != nil || body.Error.Code != code || body.RequestID != response.Header.Get("X-Request-ID") {
			t.Fatal("错误 JSON 或请求 ID 错误")
		}
		if field == "" {
			if body.Error.Details != nil {
				t.Fatal("非字段错误包含额外信息")
			}
		} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != field {
			t.Fatal("路径字段位置错误")
		}
		if status == 404 && (body.Error.Message != "会话不存在" || response.Header.Get("WWW-Authenticate") != "") {
			t.Fatal("目标错误与认证错误混淆")
		}
		if status == 401 && response.Header.Get("WWW-Authenticate") == "" {
			t.Fatal("认证错误缺少 Bearer 提示")
		}
	}
	id := func(current fixture) string { return strconv.FormatUint(current.session.ID, 10) }

	// 2. 验证目标范围、幂等、短路与失败回滚。
	t.Run("scope and repeated HTTP revocation", func(t *testing.T) {
		installation := uuid.NewString()
		caller := newFixture(t, 1, installation)
		sameDevice := newFixture(t, 1, installation)
		otherDevice := newFixture(t, 1, uuid.NewString())
		foreign := newFixture(t, 3, uuid.NewString())
		request(t, id(otherDevice), caller.access, "", 200, 1, "", "")
		firstTime, firstUpdate := readTimes(t, otherDevice.session.ID)
		request(t, id(otherDevice), caller.access, "", 200, 1, "", "")
		nextTime, nextUpdate := readTimes(t, otherDevice.session.ID)
		if !firstTime.Valid || !nextTime.Valid || !firstTime.Time.Equal(nextTime.Time) || !firstUpdate.Equal(nextUpdate) {
			t.Fatal("HTTP 重复撤销覆盖首次时间")
		}
		assertState(t, otherDevice, true)
		assertState(t, caller, false)
		assertState(t, sameDevice, false)
		assertState(t, foreign, false)
		request(t, id(sameDevice), caller.access, "", 200, 1, "", "")
		assertState(t, sameDevice, true)
		assertState(t, caller, false)
	})
	t.Run("foreign and absent targets share 404", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		foreign := newFixture(t, 3, uuid.NewString())
		request(t, id(foreign), caller.access, "", 404, 1, "SESSION_NOT_FOUND", "")
		request(t, "18446744073709551615", caller.access, "", 404, 1, "SESSION_NOT_FOUND", "")
		assertState(t, caller, false)
		assertState(t, foreign, false)
	})
	t.Run("authentication and field validation stop before service", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		request(t, "0", "", "", 401, 0, "AUTH_REQUIRED", "")
		request(t, "0", caller.access, "", 400, 0, "INVALID_ARGUMENT", "id")
		request(t, "18446744073709551616", caller.access, "", 400, 0, "INVALID_ARGUMENT", "id")
		assertState(t, caller, false)
	})
	t.Run("failure after update rolls back", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		beforeTime, beforeUpdate := readTimes(t, target.session.ID)
		request(t, id(target), caller.access, "rollback", 500, 1, "INTERNAL_ERROR", "")
		afterTime, afterUpdate := readTimes(t, target.session.ID)
		if afterTime.Valid != beforeTime.Valid || !afterUpdate.Equal(beforeUpdate) {
			t.Fatal("HTTP 失败后目标写入未回滚")
		}
		assertState(t, caller, false)
		assertState(t, target, false)
	})
	t.Run("transaction rechecks caller after authentication", func(t *testing.T) {
		for _, mode := range []string{"revoke caller", "remove caller", "expire caller", "disable account"} {
			t.Run(mode, func(t *testing.T) {
				caller := newFixture(t, 1, uuid.NewString())
				target := newFixture(t, 1, uuid.NewString())
				status, code := 401, "SESSION_REVOKED"
				if mode == "disable account" {
					status, code = 403, "ACCOUNT_DISABLED"
					defer func() {
						if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
							t.Error("恢复测试账号失败", err)
						}
					}()
				}
				request(t, id(target), caller.access, mode, status, 1, code, "")
				revokedAt, _ := readTimes(t, target.session.ID)
				if revokedAt.Valid {
					t.Fatal("调用者失效后目标仍被撤销")
				}
			})
		}
	})
	t.Run("current target and another valid caller", func(t *testing.T) {
		installation := uuid.NewString()
		caller := newFixture(t, 1, installation)
		observer := newFixture(t, 1, installation)
		request(t, id(caller), caller.access, "", 200, 1, "", "")
		assertState(t, caller, true)
		request(t, id(observer), caller.access, "", 401, 0, "SESSION_REVOKED", "")
		assertState(t, observer, false)
		request(t, id(caller), observer.access, "", 200, 1, "", "")
	})
	// 3. 等待服务结束后检查完成日志中的凭证及故障原文。
	server.Close()
	for _, secret := range append(secrets, "private-query", "private-cookie", "private-before-commit") {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("完成日志包含敏感输入或内部故障")
		}
	}
}
