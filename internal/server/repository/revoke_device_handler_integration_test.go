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

func TestRevokeDeviceHandlerIntegration(t *testing.T) {
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
	withTransaction := func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RevokeDeviceTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	}
	revoke := service.NewRevokeDeviceService(withTransaction)
	type testModeKey struct{}
	failure := errors.New("private-before-commit")
	failing := service.NewRevokeDeviceService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RevokeDeviceTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if err := action(txContext, txStore); err != nil {
				return err
			}
			if ctx.Value(testModeKey{}) == "panic" {
				panic(failure)
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
	var calls atomic.Int32
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.DELETE("/api/v1/test-revoke-devices/:id", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure),
		func(requestContext *gin.Context) {
			requestContext.Request = requestContext.Request.WithContext(context.WithValue(requestContext.Request.Context(), testModeKey{}, requestContext.GetHeader("X-Test-Mode")))
		}, handler.RevokeDevice(func(ctx context.Context, actor domain.Actor, targetID uint64) (*domain.RevokeDeviceResult, error) {
			calls.Add(1)
			// 测试模式在认证后改变本测试库状态，验证事务入口的当前读复查。
			switch ctx.Value(testModeKey{}) {
			case "rollback", "panic":
				return failing.Revoke(ctx, actor, targetID)
			case "revoke caller":
				err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return txStore.RevokeCurrentSession(txContext) })
				if err != nil {
					return nil, err
				}
			case "revoke caller device":
				err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
					return txStore.RevokeUserDevice(txContext, actor.DeviceID)
				})
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
		message, err := http.NewRequestWithContext(ctx, "DELETE", server.URL+"/api/v1/test-revoke-devices/"+target+"?user_id=99&id=99&note=private-query", strings.NewReader("{\"user_id\":\"99\",\"id\":\"99\"}"))
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
			response.Header.Get("X-Request-ID") == "" || response.Header.Get("Cache-Control") != "no-store" || len(response.Cookies()) != 0 ||
			response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" ||
			response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
			t.Fatal("网络状态、服务调用或响应头错误", response.StatusCode, calls.Load()-before, readError, closeError)
		}
		if status == 200 {
			var body dto.RevokeDeviceResponse
			if json.Unmarshal(encoded, &body) != nil || !body.Data.Revoked || body.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" || response.ContentLength != int64(len(encoded)) {
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
		if status == 404 && (body.Error.Message != "设备不存在" || response.Header.Get("WWW-Authenticate") != "") {
			t.Fatal("目标错误与认证错误混淆")
		}
		if status == 401 && response.Header.Get("WWW-Authenticate") == "" {
			t.Fatal("认证错误缺少 Bearer 提示")
		}
	}
	id := func(current fixture) string { return strconv.FormatUint(current.session.DeviceID, 10) }

	readDeviceTimes := func(t *testing.T, deviceID uint64) (sql.NullTime, time.Time) {
		t.Helper()
		var revokedAt sql.NullTime
		var updatedAt time.Time
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at,updated_at FROM user_devices WHERE id=?", deviceID).Scan(&revokedAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		return revokedAt, updatedAt
	}

	// 2. 验证设备与全部会话共同提交，重试保留首次时间，重新登录复用设备。
	t.Run("scope repeated revocation and restored login", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		installation := uuid.NewString()
		target := newFixture(t, 1, installation)
		sameDevice := newFixture(t, 1, installation)
		foreign := newFixture(t, 3, installation)
		request(t, id(target), caller.access, "", 200, 1, "", "")
		deviceTime, deviceUpdate := readDeviceTimes(t, target.session.DeviceID)
		if !deviceTime.Valid || !deviceUpdate.Equal(deviceTime.Time) {
			t.Fatal("成功 HTTP 返回后设备撤销未确认提交")
		}
		sessionTimes, sessionUpdates := make([]sql.NullTime, 2), make([]time.Time, 2)
		for index, current := range []fixture{target, sameDevice} {
			assertState(t, current, true)
			sessionTimes[index], sessionUpdates[index] = readTimes(t, current.session.ID)
			if !sessionTimes[index].Valid {
				t.Fatal("成功 HTTP 返回后关联会话撤销未提交")
			}
		}
		request(t, id(target), caller.access, "", 200, 1, "", "")
		againTime, againUpdate := readDeviceTimes(t, target.session.DeviceID)
		if againTime != deviceTime || !againUpdate.Equal(deviceUpdate) {
			t.Fatal("HTTP 重试覆盖设备首次撤销时间")
		}
		for index, current := range []fixture{target, sameDevice} {
			revokedAt, updatedAt := readTimes(t, current.session.ID)
			if revokedAt != sessionTimes[index] || !updatedAt.Equal(sessionUpdates[index]) {
				t.Fatal("HTTP 重试覆盖会话历史时间")
			}
		}
		assertState(t, caller, false)
		assertState(t, foreign, false)
		restored := newFixture(t, 1, installation)
		if restored.session.DeviceID != target.session.DeviceID || restored.session.ID == target.session.ID {
			t.Fatal("重新登录未复用原设备并创建新会话")
		}
		assertState(t, restored, false)
		assertState(t, target, true)
		assertState(t, sameDevice, true)
	})
	t.Run("foreign and absent devices share 404", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		foreign := newFixture(t, 3, uuid.NewString())
		request(t, id(foreign), caller.access, "", 404, 1, "DEVICE_NOT_FOUND", "")
		request(t, "18446744073709551615", caller.access, "", 404, 1, "DEVICE_NOT_FOUND", "")
		assertState(t, caller, false)
		assertState(t, foreign, false)
		deviceTime, _ := readDeviceTimes(t, foreign.session.DeviceID)
		if deviceTime.Valid {
			t.Fatal("跨账号请求修改了目标设备")
		}
	})
	t.Run("authentication and field validation stop before service", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		request(t, "0", "", "", 401, 0, "AUTH_REQUIRED", "")
		request(t, "0", caller.access, "", 400, 0, "INVALID_ARGUMENT", "id")
		request(t, "18446744073709551616", caller.access, "", 400, 0, "INVALID_ARGUMENT", "id")
		assertState(t, caller, false)
	})

	// 3. 验证故障与 panic 回滚两张表，并复查认证后发生的调用者状态变化。
	t.Run("failure and panic after updates roll back both tables", func(t *testing.T) {
		for _, mode := range []string{"rollback", "panic"} {
			t.Run(mode, func(t *testing.T) {
				caller := newFixture(t, 1, uuid.NewString())
				installation := uuid.NewString()
				target := newFixture(t, 1, installation)
				sameDevice := newFixture(t, 1, installation)
				deviceBefore, deviceUpdateBefore := readDeviceTimes(t, target.session.DeviceID)
				sessionTimes, sessionUpdates := make([]sql.NullTime, 2), make([]time.Time, 2)
				for index, current := range []fixture{target, sameDevice} {
					sessionTimes[index], sessionUpdates[index] = readTimes(t, current.session.ID)
				}
				request(t, id(target), caller.access, mode, 500, 1, "INTERNAL_ERROR", "")
				deviceAfter, deviceUpdateAfter := readDeviceTimes(t, target.session.DeviceID)
				if deviceAfter != deviceBefore || !deviceUpdateAfter.Equal(deviceUpdateBefore) {
					t.Fatal("HTTP 失败后设备写入未回滚")
				}
				for index, current := range []fixture{target, sameDevice} {
					revokedAt, updatedAt := readTimes(t, current.session.ID)
					if revokedAt != sessionTimes[index] || !updatedAt.Equal(sessionUpdates[index]) {
						t.Fatal("HTTP 失败后关联会话写入未回滚")
					}
					assertState(t, current, false)
				}
				assertState(t, caller, false)
			})
		}
	})
	t.Run("transaction rechecks caller after authentication", func(t *testing.T) {
		for _, targetUserID := range []uint64{1, 3} {
			for _, mode := range []string{"revoke caller", "remove caller", "expire caller", "revoke caller device", "disable account"} {
				t.Run(strconv.FormatUint(targetUserID, 10)+"/"+mode, func(t *testing.T) {
					caller := newFixture(t, 1, uuid.NewString())
					target := newFixture(t, targetUserID, uuid.NewString())
					status, code := 401, "SESSION_REVOKED"
					if mode == "disable account" {
						status, code = 403, "ACCOUNT_DISABLED"
						defer func() {
							if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
								t.Error("恢复测试账号失败", err)
							}
						}()
					}
					deviceBefore, deviceUpdateBefore := readDeviceTimes(t, target.session.DeviceID)
					sessionBefore, sessionUpdateBefore := readTimes(t, target.session.ID)
					// 本账号和跨账号目标均先复查调用者的当前认证状态。
					request(t, id(target), caller.access, mode, status, 1, code, "")
					deviceAfter, deviceUpdateAfter := readDeviceTimes(t, target.session.DeviceID)
					sessionAfter, sessionUpdateAfter := readTimes(t, target.session.ID)
					if deviceAfter != deviceBefore || !deviceUpdateAfter.Equal(deviceUpdateBefore) || sessionAfter != sessionBefore || !sessionUpdateAfter.Equal(sessionUpdateBefore) {
						t.Fatal("调用者失效后目标发生变化")
					}
				})
			}
		}
	})
	t.Run("current device invalidates all its sessions", func(t *testing.T) {
		installation := uuid.NewString()
		caller := newFixture(t, 1, installation)
		sameDevice := newFixture(t, 1, installation)
		observer := newFixture(t, 1, uuid.NewString())
		request(t, id(caller), caller.access, "", 200, 1, "", "")
		assertState(t, caller, true)
		assertState(t, sameDevice, true)
		request(t, id(observer), caller.access, "", 401, 0, "SESSION_REVOKED", "")
		request(t, id(observer), sameDevice.access, "", 401, 0, "SESSION_REVOKED", "")
		assertState(t, observer, false)
		request(t, id(caller), observer.access, "", 200, 1, "", "")
	})
	t.Run("already revoked device repairs remaining sessions", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at='2020-01-02 03:04:05.123456',updated_at='2020-01-02 03:04:05.123456' WHERE id=?", target.session.DeviceID); err != nil {
			t.Fatal(err)
		}
		beforeTime, beforeUpdate := readDeviceTimes(t, target.session.DeviceID)
		request(t, id(target), caller.access, "", 200, 1, "", "")
		afterTime, afterUpdate := readDeviceTimes(t, target.session.DeviceID)
		sessionTime, _ := readTimes(t, target.session.ID)
		if afterTime != beforeTime || !afterUpdate.Equal(beforeUpdate) || !sessionTime.Valid {
			t.Fatal("历史设备撤销时间或关联会话补齐错误")
		}
		assertState(t, target, true)
		assertState(t, caller, false)
	})

	// 4. 等待测试服务关闭后检查完成日志中的敏感值。
	server.Close()
	for _, secret := range append(secrets, "private-query", "private-cookie", "private-before-commit") {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("完成日志包含敏感输入或内部故障")
		}
	}
}
