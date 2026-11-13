package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRenameDeviceServiceIntegration(t *testing.T) {
	// 1. 复用独占库和正式设备/会话表，建立事务所需的有效身份。
	pool := loginSessionDatabase(t)
	pool.SetMaxOpenConns(4)
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
	newActor := func(t *testing.T, userID uint64, installation string) domain.Actor {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
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
			t.Fatal("创建改名测试会话失败", err)
		}
		token, err := signer.Issue(*session)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("取得真实 JWT 认证身份失败", err)
		}
		return *actor
	}
	type snapshot struct {
		installation, name, platform string
		createdAt, updatedAt         time.Time
		lastSeenAt, revokedAt        sql.NullTime
		osVersion, appVersion        sql.NullString
	}
	read := func(t *testing.T, deviceID uint64) snapshot {
		t.Helper()
		var value snapshot
		err := pool.QueryRowContext(ctx, "SELECT installation_id,name,platform,created_at,updated_at,last_seen_at,revoked_at,os_version,app_version FROM user_devices WHERE id=?", deviceID).
			Scan(&value.installation, &value.name, &value.platform, &value.createdAt, &value.updatedAt, &value.lastSeenAt, &value.revokedAt, &value.osVersion, &value.appVersion)
		if err != nil {
			t.Fatal("读取设备快照失败", err)
		}
		return value
	}
	rename := service.NewRenameDeviceService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RenameDeviceTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	// 2. 服务成功返回时，已能从另一连接读取提交后的名称。
	t.Run("committed result current target and idempotency", func(t *testing.T) {
		installation := uuid.NewString()
		caller := newActor(t, 1, installation)
		sameDevice := newActor(t, 1, installation)
		target := newActor(t, 1, uuid.NewString())
		changedAt := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=?,last_seen_at=?,updated_at=? WHERE id=?", changedAt, changedAt, changedAt, target.DeviceID); err != nil {
			t.Fatal(err)
		}
		original := read(t, target.DeviceID)
		name := "  学习设备 🎮  "
		result, err := rename.Rename(ctx, caller, target.DeviceID, name)
		if err != nil || result == nil || result.ID != target.DeviceID || result.UserID != caller.UserID || result.Name != name || result.RevokedAt == nil || !result.RevokedAt.Equal(changedAt) || result.LastSeenAt == nil || !result.LastSeenAt.Equal(changedAt) {
			t.Fatal("服务改名结果或历史事实错误", err)
		}
		committed := read(t, target.DeviceID)
		expected := original
		expected.name, expected.updatedAt = name, committed.updatedAt
		if !reflect.DeepEqual(committed, expected) || !committed.updatedAt.After(changedAt) {
			t.Fatal("服务返回时未确认设备改名提交")
		}
		result.Name = "caller copy"
		*result.RevokedAt = result.RevokedAt.Add(time.Hour)
		repeated, err := rename.Rename(ctx, caller, target.DeviceID, name)
		if err != nil || repeated == nil || repeated.Name != name || !repeated.RevokedAt.Equal(changedAt) || !reflect.DeepEqual(read(t, target.DeviceID), committed) {
			t.Fatal("重复改名或结果副本错误", err)
		}
		current, err := rename.Rename(ctx, sameDevice, caller.DeviceID, "当前设备新名")
		if err != nil || current == nil || current.ID != caller.DeviceID || read(t, caller.DeviceID).name != current.Name {
			t.Fatal("同设备另一会话改名失败", err)
		}
		if _, err := rename.Rename(ctx, caller, caller.DeviceID, "仍有效的登录设备"); err != nil {
			t.Fatal("设备改名影响了当前会话有效性", err)
		}
	})
	t.Run("target error classification", func(t *testing.T) {
		caller := newActor(t, 1, uuid.NewString())
		foreign := newActor(t, 3, uuid.NewString())
		original := read(t, foreign.DeviceID)
		for _, identifier := range []uint64{foreign.DeviceID, ^uint64(0)} {
			result, err := rename.Rename(ctx, caller, identifier, "其他名称")
			if result != nil || !errors.Is(err, domain.ErrTargetDeviceNotFound) || errors.Is(err, domain.ErrDeviceNotFound) {
				t.Fatal("目标不存在或跨账号错误分类错误", err)
			}
		}
		if !reflect.DeepEqual(read(t, foreign.DeviceID), original) {
			t.Fatal("改名影响了其他账号设备")
		}
	})
	// 3. 外层故障与取消发生在回调之后时，服务返回空结果且数据库回滚。
	t.Run("rollback after prepared result", func(t *testing.T) {
		caller, target := newActor(t, 1, uuid.NewString()), newActor(t, 1, uuid.NewString())
		baseline := read(t, target.DeviceID)
		failure := errors.New("service callback boundary fault")
		for _, mode := range []string{"error", "cancel"} {
			t.Run(mode, func(t *testing.T) {
				requestContext, stop := context.WithCancel(ctx)
				defer stop()
				failing := service.NewRenameDeviceService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RenameDeviceTransaction) error) error {
					return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
						if err := action(txContext, txStore); err != nil {
							return err
						}
						if mode == "cancel" {
							stop()
							return nil
						}
						return failure
					})
				})
				result, err := failing.Rename(requestContext, caller, target.DeviceID, "回滚的新名")
				want := error(failure)
				if mode == "cancel" {
					want = context.Canceled
				}
				if result != nil || !errors.Is(err, want) || !reflect.DeepEqual(read(t, target.DeviceID), baseline) {
					t.Fatal("暂存结果在回滚后仍被交付", err)
				}
			})
		}
	})
	t.Run("caller errors precede target classification", func(t *testing.T) {
		for _, mode := range []string{"missing session", "revoked session", "revoked device", "expired session", "disabled account"} {
			t.Run(mode, func(t *testing.T) {
				caller, foreign := newActor(t, 1, uuid.NewString()), newActor(t, 3, uuid.NewString())
				baseline := read(t, foreign.DeviceID)
				var statement string
				identifier := caller.SessionID
				var want error
				switch mode {
				case "missing session":
					statement, want = "DELETE FROM auth_sessions WHERE id=?", domain.ErrSessionNotFound
				case "revoked session":
					statement, want = "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", domain.ErrSessionRevoked
				case "revoked device":
					statement, identifier, want = "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", caller.DeviceID, domain.ErrDeviceRevoked
				case "expired session":
					statement, want = "UPDATE auth_sessions SET expires_at=DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 1 MINUTE) WHERE id=?", domain.ErrSessionExpired
				default:
					statement, identifier, want = "UPDATE users SET status='DISABLED' WHERE id=?", caller.UserID, domain.ErrAccountDisabled
					defer func() {
						if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=?", caller.UserID); err != nil {
							t.Error("恢复独占库账号失败", err)
						}
					}()
				}
				if _, err := pool.ExecContext(ctx, statement, identifier); err != nil {
					t.Fatal(err)
				}
				result, err := rename.Rename(ctx, caller, foreign.DeviceID, "无权改名")
				if result != nil || !errors.Is(err, want) || errors.Is(err, domain.ErrTargetDeviceNotFound) || !reflect.DeepEqual(read(t, foreign.DeviceID), baseline) {
					t.Fatal("调用者状态被目标错误覆盖", err)
				}
			})
		}
	})
	t.Run("confirmed commit then cancellation", func(t *testing.T) {
		caller, target := newActor(t, 1, uuid.NewString()), newActor(t, 1, uuid.NewString())
		requestContext, stop := context.WithCancel(ctx)
		defer stop()
		committed := service.NewRenameDeviceService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RenameDeviceTransaction) error) error {
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return action(txContext, txStore)
			})
			if err == nil {
				stop()
			}
			return err
		})
		result, err := committed.Rename(requestContext, caller, target.DeviceID, "提交后名称")
		if err != nil || result == nil || result.Name != "提交后名称" || requestContext.Err() == nil || read(t, target.DeviceID).name != result.Name {
			t.Fatal("已提交改名结果被后续取消覆盖", err)
		}
	})
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("服务关闭了共享连接池", err)
	}
}
