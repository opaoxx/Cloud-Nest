package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRevokeDeviceServiceIntegration(t *testing.T) {
	// 1. 复用独占库，装配真实 JWT、刷新校验及已认证事务适配器。
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
	type fixture struct {
		actor           domain.Actor
		access, refresh string
	}
	newFixture := func(t *testing.T, userID uint64, installation string) fixture {
		t.Helper()
		refresh, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installation, "设备撤销服务测试", "WEB")
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
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("夹具授权失败", err)
		}
		return fixture{actor: *actor, access: token.Token, refresh: refresh}
	}
	assertState := func(t *testing.T, current fixture, revoked bool) {
		t.Helper()
		actor, accessError := authorization.Authorize(ctx, current.access)
		verified, refreshError := credentials.Verify(ctx, current.refresh)
		if revoked {
			if actor != nil || verified != nil || !errors.Is(accessError, domain.ErrSessionRevoked) || !errors.Is(refreshError, domain.ErrSessionRevoked) {
				t.Fatal("已提交撤销后凭证仍有效", accessError, refreshError)
			}
		} else if accessError != nil || refreshError != nil || actor == nil || *actor != current.actor || verified == nil || verified.Session.ID != current.actor.SessionID {
			t.Fatal("有效会话状态异常", accessError, refreshError)
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

	// 2. 验证提交、幂等及调用者/目标错误的分界。
	readDeviceTimes := func(t *testing.T, deviceID uint64) (sql.NullTime, time.Time) {
		t.Helper()
		var revokedAt sql.NullTime
		var updatedAt time.Time
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at,updated_at FROM user_devices WHERE id=?", deviceID).Scan(&revokedAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		return revokedAt, updatedAt
	}
	t.Run("committed device and sessions with repeated results", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		installation := uuid.NewString()
		target := newFixture(t, 1, installation)
		sameDevice := newFixture(t, 1, installation)
		foreign := newFixture(t, 3, installation)
		result, err := revoke.Revoke(ctx, caller.actor, target.actor.DeviceID)
		if err != nil || result == nil || !result.Revoked {
			t.Fatal("设备撤销服务提交失败", err)
		}
		firstTime, firstUpdate := readDeviceTimes(t, target.actor.DeviceID)
		if !firstTime.Valid || !firstUpdate.Equal(firstTime.Time) {
			t.Fatal("服务返回时设备撤销未提交")
		}
		sessionTimes, sessionUpdates := make([]sql.NullTime, 2), make([]time.Time, 2)
		for index, current := range []fixture{target, sameDevice} {
			assertState(t, current, true)
			sessionTimes[index], sessionUpdates[index] = readTimes(t, current.actor.SessionID)
		}
		result.Revoked = false
		again, err := revoke.Revoke(ctx, caller.actor, target.actor.DeviceID)
		nextTime, nextUpdate := readDeviceTimes(t, target.actor.DeviceID)
		if err != nil || again == nil || !again.Revoked || again == result || nextTime != firstTime || !nextUpdate.Equal(firstUpdate) {
			t.Fatal("重复设备撤销结果或首次时间错误", err)
		}
		for index, current := range []fixture{target, sameDevice} {
			revokedAt, updatedAt := readTimes(t, current.actor.SessionID)
			if revokedAt != sessionTimes[index] || !updatedAt.Equal(sessionUpdates[index]) {
				t.Fatal("重复设备撤销覆盖会话历史时间")
			}
		}
		assertState(t, caller, false)
		assertState(t, foreign, false)
		restored := newFixture(t, 1, installation)
		if restored.actor.DeviceID != target.actor.DeviceID || restored.actor.SessionID == target.actor.SessionID {
			t.Fatal("恢复登录未复用设备或创建新会话")
		}
		assertState(t, restored, false)
		assertState(t, target, true)
		assertState(t, sameDevice, true)
	})
	t.Run("missing and foreign targets use target error", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		foreign := newFixture(t, 3, uuid.NewString())
		for _, targetID := range []uint64{foreign.actor.DeviceID, ^uint64(0)} {
			result, err := revoke.Revoke(ctx, caller.actor, targetID)
			if result != nil || !errors.Is(err, domain.ErrTargetDeviceNotFound) || errors.Is(err, domain.ErrDeviceNotFound) || errors.Is(err, domain.ErrSessionNotFound) {
				t.Fatal("目标设备不存在的错误分类不准确", err)
			}
		}
		assertState(t, caller, false)
		assertState(t, foreign, false)
	})
	t.Run("caller state errors remain authentication errors", func(t *testing.T) {
		for _, testCase := range []struct {
			name, statement string
			want            error
		}{
			{name: "missing session", statement: "DELETE FROM auth_sessions WHERE id=?", want: domain.ErrSessionNotFound},
			{name: "revoked session", statement: "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", want: domain.ErrSessionRevoked},
			{name: "expired session", statement: "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=?", want: domain.ErrSessionExpired},
			{name: "revoked device", statement: "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", want: domain.ErrDeviceRevoked},
			{name: "disabled account", statement: "UPDATE users SET status='DISABLED' WHERE id=?", want: domain.ErrAccountDisabled},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				caller := newFixture(t, 1, uuid.NewString())
				target := newFixture(t, 3, uuid.NewString())
				identifier := caller.actor.SessionID
				if testCase.name == "revoked device" {
					identifier = caller.actor.DeviceID
				}
				if testCase.name == "disabled account" {
					identifier = caller.actor.UserID
					defer func() {
						if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=?", identifier); err != nil {
							t.Error("恢复测试账号状态失败", err)
						}
					}()
				}
				if _, err := pool.ExecContext(ctx, testCase.statement, identifier); err != nil {
					t.Fatal(err)
				}
				result, err := revoke.Revoke(ctx, caller.actor, target.actor.DeviceID)
				if result != nil || !errors.Is(err, testCase.want) || errors.Is(err, domain.ErrTargetDeviceNotFound) {
					t.Fatal("调用者状态错误被转换成目标错误", err)
				}
				assertState(t, target, false)
			})
		}
	})
	t.Run("current target and stale caller", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		result, err := revoke.Revoke(ctx, caller.actor, caller.actor.DeviceID)
		if err != nil || result == nil || !result.Revoked {
			t.Fatal("当前设备撤销失败", err)
		}
		assertState(t, caller, true)
		result, err = revoke.Revoke(ctx, caller.actor, target.actor.DeviceID)
		if result != nil || !errors.Is(err, domain.ErrSessionRevoked) || errors.Is(err, domain.ErrTargetDeviceNotFound) {
			t.Fatal("失效调用者继续执行撤销", err)
		}
		assertState(t, target, false)
	})

	// 3. 验证回调完成后提交前故障，以及提交确认后的取消。
	t.Run("failure cancellation and panic roll back", func(t *testing.T) {
		for _, mode := range []string{"error", "cancel", "panic"} {
			t.Run(mode, func(t *testing.T) {
				caller := newFixture(t, 1, uuid.NewString())
				target := newFixture(t, 1, uuid.NewString())
				beforeTime, beforeUpdate := readTimes(t, target.actor.SessionID)
				deviceBefore, deviceUpdateBefore := readDeviceTimes(t, target.actor.DeviceID)
				requestContext, cancelRequest := context.WithCancel(ctx)
				defer cancelRequest()
				failure := errors.New("fixture before commit")
				failing := service.NewRevokeDeviceService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RevokeDeviceTransaction) error) error {
					return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
						if err := action(txContext, txStore); err != nil {
							return err
						}
						switch mode {
						case "cancel":
							cancelRequest()
							return nil
						case "panic":
							panic(failure)
						default:
							return failure
						}
					})
				})
				if mode == "panic" {
					func() {
						defer func() {
							if recover() != failure {
								t.Error("原 panic 未传播")
							}
						}()
						_, _ = failing.Revoke(requestContext, caller.actor, target.actor.DeviceID)
					}()
				} else {
					want := failure
					if mode == "cancel" {
						want = context.Canceled
					}
					result, err := failing.Revoke(requestContext, caller.actor, target.actor.DeviceID)
					if result != nil || !errors.Is(err, want) {
						t.Fatal("事务失败返回了成功结果或丢失原因", err)
					}
				}
				afterTime, afterUpdate := readTimes(t, target.actor.SessionID)
				deviceAfter, deviceUpdateAfter := readDeviceTimes(t, target.actor.DeviceID)
				if deviceAfter != deviceBefore || !deviceUpdateAfter.Equal(deviceUpdateBefore) {
					t.Fatal("服务失败后设备撤销未回滚")
				}
				if afterTime.Valid != beforeTime.Valid || !afterUpdate.Equal(beforeUpdate) {
					t.Fatal("回滚后目标的撤销时间或更新时间被保存")
				}
				assertState(t, caller, false)
				assertState(t, target, false)
			})
		}
	})
	t.Run("cancel after confirmed commit retains result", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		requestContext, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		confirmed := service.NewRevokeDeviceService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RevokeDeviceTransaction) error) error {
			err := withTransaction(ctx, actor, action)
			cancelRequest()
			return err
		})
		result, err := confirmed.Revoke(requestContext, caller.actor, target.actor.DeviceID)
		if err != nil || result == nil || !result.Revoked || requestContext.Err() == nil {
			t.Fatal("确认提交后的结果丢失", err)
		}
		deviceRevokedAt, _ := readDeviceTimes(t, target.actor.DeviceID)
		if !deviceRevokedAt.Valid {
			t.Fatal("已提交结果缺少设备撤销事实")
		}
		assertState(t, target, true)
		assertState(t, caller, false)
	})
	t.Run("concurrent valid callers both receive committed results", func(t *testing.T) {
		callers := []fixture{newFixture(t, 1, uuid.NewString()), newFixture(t, 1, uuid.NewString())}
		target := newFixture(t, 1, uuid.NewString())
		type outcome struct {
			result *domain.RevokeDeviceResult
			err    error
		}
		results := make(chan outcome, 2)
		for _, caller := range callers {
			go func(actor domain.Actor) {
				result, err := revoke.Revoke(ctx, actor, target.actor.DeviceID)
				results <- outcome{result: result, err: err}
			}(caller.actor)
		}
		for range 2 {
			select {
			case received := <-results:
				if received.err != nil || received.result == nil || !received.result.Revoked {
					t.Fatal("有效调用者并发撤销未取得提交结果", received.err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("并发撤销未结束")
			}
		}
		assertState(t, target, true)
		for _, caller := range callers {
			assertState(t, caller, false)
		}
	})
}
