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
	"reflect"
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

func TestRenameDeviceHandlerIntegration(t *testing.T) {
	// 1. 复用独占库，装配真实认证、改名服务、事务及 HTTP 处理链。
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
	withTransaction := func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RenameDeviceTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	}
	rename := service.NewRenameDeviceService(withTransaction)
	failure := errors.New("private-before-commit")
	failing := service.NewRenameDeviceService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RenameDeviceTransaction) error) error {
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
			device, err := txStore.EnsureLoginDevice(txContext, installation, "原设备", "WEB")
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
	type deviceSnapshot struct {
		name, installation, platform string
		createdAt, updatedAt         time.Time
		lastSeenAt, revokedAt        sql.NullTime
	}
	read := func(t *testing.T, deviceID uint64) deviceSnapshot {
		t.Helper()
		var value deviceSnapshot
		if err := pool.QueryRowContext(ctx, "SELECT name,installation_id,platform,created_at,updated_at,last_seen_at,revoked_at FROM user_devices WHERE id=?", deviceID).Scan(&value.name, &value.installation, &value.platform, &value.createdAt, &value.updatedAt, &value.lastSeenAt, &value.revokedAt); err != nil {
			t.Fatal(err)
		}
		return value
	}
	type testModeKey struct{}
	var calls atomic.Int32
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.PATCH("/api/v1/test-rename-devices/:id", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure),
		func(requestContext *gin.Context) {
			requestContext.Request = requestContext.Request.WithContext(context.WithValue(requestContext.Request.Context(), testModeKey{}, requestContext.GetHeader("X-Test-Mode")))
		}, handler.RenameDevice(func(ctx context.Context, actor domain.Actor, targetID uint64, name string) (*domain.UserDevice, error) {
			calls.Add(1)
			// 测试模式在认证后改变本测试库状态，验证事务入口的当前读复查。
			switch ctx.Value(testModeKey{}) {
			case "rollback":
				return failing.Rename(ctx, actor, targetID, name)
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
			case "revoke device":
				if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE user_id=? AND id=?", actor.UserID, actor.DeviceID); err != nil {
					return nil, err
				}
			case "disable account":
				if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=?", actor.UserID); err != nil {
					return nil, err
				}
			}
			return rename.Rename(ctx, actor, targetID, name)
		}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	request := func(t *testing.T, target, token, body, mode string, status, serviceCalls int, code, field string) dto.RenameDeviceResponse {
		t.Helper()
		before := calls.Load()
		message, err := http.NewRequestWithContext(ctx, "PATCH", server.URL+"/api/v1/test-rename-devices/"+target+"?user_id=99&id=99&name=private-query", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		message.Header.Set("Content-Type", "application/json; charset=utf-8")
		message.Header.Set("Origin", "http://127.0.0.1:5173")
		message.Header.Set("X-Test-Mode", mode)
		message.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-cookie"})
		if token != "" {
			message.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(message)
		if err != nil {
			t.Fatal("改名 HTTP 请求失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || int(calls.Load()-before) != serviceCalls ||
			response.Header.Get("X-Request-ID") == "" || response.Header.Get("Cache-Control") != "no-store" || len(response.Cookies()) != 0 {
			t.Fatal("网络状态、服务调用或响应头错误", response.StatusCode, readError, closeError)
		}
		var payload dto.RenameDeviceResponse
		if status != 200 {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("改名错误 JSON 或请求 ID 错误")
			}
			if field == "" {
				if failure.Error.Details != nil {
					t.Fatal("非字段错误包含额外信息")
				}
			} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
				t.Fatal("改名字段错误位置不准确")
			}
			if status == 404 && (failure.Error.Message != "设备不存在" || response.Header.Get("WWW-Authenticate") != "") {
				t.Fatal("目标错误与认证错误混淆")
			}
			if status == 401 && response.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("认证错误缺少 Bearer 提示")
			}
			return payload
		}
		var input dto.RenameDeviceRequest
		if json.Unmarshal([]byte(body), &input) != nil || json.Unmarshal(encoded, &payload) != nil || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("成功 JSON 或缓存头错误")
		}
		device := payload.Data.Device
		identifier, err := strconv.ParseUint(device.ID, 10, 64)
		if err != nil || device.ID != target || device.Name != input.Name {
			t.Fatal("改名目标或名称原文错误", err)
		}
		identity, err := verifier.Verify(token)
		if err != nil || identity == nil {
			t.Fatal(err)
		}
		var owner uint64
		if err := pool.QueryRowContext(ctx, "SELECT user_id FROM user_devices WHERE id=?", identifier).Scan(&owner); err != nil || owner != identity.UserID {
			t.Fatal("响应设备与认证归属不符", err)
		}
		committed := read(t, identifier)
		if committed.name != device.Name || committed.platform != device.Platform || !committed.createdAt.Equal(device.CreatedAt) || device.CreatedAt.Location() != time.UTC || device.CreatedAt.Nanosecond()%1000 != 0 ||
			committed.lastSeenAt.Valid != (device.LastSeenAt != nil) || committed.revokedAt.Valid != (device.RevokedAt != nil) {
			t.Fatal("返回设备与数据库提交事实不一致")
		}
		if (committed.lastSeenAt.Valid && !device.LastSeenAt.Equal(committed.lastSeenAt.Time)) || (committed.revokedAt.Valid && !device.RevokedAt.Equal(committed.revokedAt.Time)) {
			t.Fatal("设备可空时间事实不一致")
		}
		return payload
	}
	id := func(current fixture) string { return strconv.FormatUint(current.session.DeviceID, 10) }
	const validBody = `{"name":"  学习电脑 🎮  "}`
	// 2. 验证完整请求链的归属、重复修改、历史状态和参数短路。
	t.Run("owned current and revoked devices", func(t *testing.T) {
		installation := uuid.NewString()
		caller := newFixture(t, 1, installation)
		sameDevice := newFixture(t, 1, installation)
		target := newFixture(t, 1, uuid.NewString())
		foreign := newFixture(t, 3, installation)
		before := read(t, target.session.DeviceID)
		request(t, id(target), caller.access, validBody, "", 200, 1, "", "")
		first := read(t, target.session.DeviceID)
		expected := before
		expected.name, expected.updatedAt = "  学习电脑 🎮  ", first.updatedAt
		if !reflect.DeepEqual(first, expected) {
			t.Fatal("改名写入了预期之外的字段")
		}
		request(t, id(target), caller.access, validBody, "", 200, 1, "", "")
		if !reflect.DeepEqual(read(t, target.session.DeviceID), first) {
			t.Fatal("同名重试改变了更新时间或其他字段")
		}
		request(t, id(caller), sameDevice.access, validBody, "", 200, 1, "", "")
		assertState(t, caller, false)
		assertState(t, sameDevice, false)
		assertState(t, foreign, false)
		changedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=?,last_seen_at=? WHERE id=?", changedAt, changedAt, target.session.DeviceID); err != nil {
			t.Fatal(err)
		}
		request(t, id(target), caller.access, `{"name":"历史设备新名"}`, "", 200, 1, "", "")
		if !read(t, target.session.DeviceID).revokedAt.Time.Equal(changedAt) {
			t.Fatal("改名改变了撤销事实")
		}
	})
	t.Run("missing and foreign targets share 404", func(t *testing.T) {
		caller, foreign := newFixture(t, 1, uuid.NewString()), newFixture(t, 3, uuid.NewString())
		before := read(t, foreign.session.DeviceID)
		request(t, id(foreign), caller.access, validBody, "", 404, 1, "DEVICE_NOT_FOUND", "")
		request(t, "18446744073709551615", caller.access, validBody, "", 404, 1, "DEVICE_NOT_FOUND", "")
		if !reflect.DeepEqual(read(t, foreign.session.DeviceID), before) {
			t.Fatal("改名影响了其他账号设备")
		}
	})
	t.Run("authentication and input validation stop service", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		request(t, "0", "", `{"name":`, "", 401, 0, "AUTH_REQUIRED", "")
		request(t, "0", caller.access, `{"name":`, "", 400, 0, "INVALID_ARGUMENT", "id")
		request(t, "18446744073709551616", caller.access, validBody, "", 400, 0, "INVALID_ARGUMENT", "id")
		request(t, id(caller), caller.access, `{"name":"private-name\n"}`, "", 400, 0, "INVALID_ARGUMENT", "name")
		request(t, id(caller), caller.access, `{"name":`, "", 400, 0, "INVALID_ARGUMENT", "")
		request(t, id(caller), caller.access, `{"name":"学习设备","user_id":"private-user"}`, "", 400, 0, "INVALID_ARGUMENT", "")
		assertState(t, caller, false)
	})
	t.Run("failure after update rolls back", func(t *testing.T) {
		caller, target := newFixture(t, 1, uuid.NewString()), newFixture(t, 1, uuid.NewString())
		before := read(t, target.session.DeviceID)
		request(t, id(target), caller.access, validBody, "rollback", 500, 1, "INTERNAL_ERROR", "")
		if !reflect.DeepEqual(read(t, target.session.DeviceID), before) {
			t.Fatal("HTTP 失败后名称更新未回滚")
		}
		assertState(t, caller, false)
		assertState(t, target, false)
	})
	t.Run("transaction rechecks caller after authentication", func(t *testing.T) {
		for _, mode := range []string{"revoke caller", "remove caller", "expire caller", "revoke device", "disable account"} {
			t.Run(mode, func(t *testing.T) {
				caller, target := newFixture(t, 1, uuid.NewString()), newFixture(t, 1, uuid.NewString())
				before := read(t, target.session.DeviceID)
				status, code := 401, "SESSION_REVOKED"
				if mode == "disable account" {
					status, code = 403, "ACCOUNT_DISABLED"
					defer func() {
						if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
							t.Error("恢复独占库账号失败", err)
						}
					}()
				}
				request(t, id(target), caller.access, validBody, mode, status, 1, code, "")
				if !reflect.DeepEqual(read(t, target.session.DeviceID), before) {
					t.Fatal("认证后身份失效仍修改了目标设备")
				}
			})
		}
	})
	// 3. 服务关闭后核对完成日志中的凭证、正文与故障原文保护。
	server.Close()
	for _, secret := range append(secrets, "private-query", "private-cookie", "private-before-commit", "private-name", "private-user") {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("完成日志包含敏感输入或内部故障")
		}
	}
}
