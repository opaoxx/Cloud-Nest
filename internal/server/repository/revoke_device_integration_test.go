package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRevokeUserDeviceIntegration(t *testing.T) {
	// 1. 在独占库中装配真实事务、JWT 授权和刷新凭证校验。
	pool := loginSessionDatabase(t)
	pool.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	installSyncDeviceFixtureTable(t, ctx, pool)
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
	type fixture struct {
		actor           domain.Actor
		access, refresh string
	}
	issueFixture := func(t *testing.T, session domain.LoginSession, refresh string) fixture {
		t.Helper()
		token, err := signer.Issue(session)
		if err != nil {
			t.Fatal("签发测试访问令牌失败", err)
		}
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("夹具授权失败", err)
		}
		return fixture{actor: *actor, access: token.Token, refresh: refresh}
	}
	newFixture := func(t *testing.T, userID uint64, installation string) fixture {
		t.Helper()
		refresh, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installation, "设备撤销测试", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		})
		if err != nil || session == nil {
			t.Fatal("创建测试会话失败", err)
		}
		return issueFixture(t, *session, refresh)
	}
	assertCredentials := func(t *testing.T, current fixture, want error) {
		t.Helper()
		actor, accessError := authorization.Authorize(ctx, current.access)
		verified, refreshError := credentials.Verify(ctx, current.refresh)
		if want == nil {
			if accessError != nil || refreshError != nil || actor == nil || *actor != current.actor || verified == nil || verified.Session.ID != current.actor.SessionID {
				t.Fatal("有效访问或刷新凭证异常", accessError, refreshError)
			}
		} else if actor != nil || verified != nil || !errors.Is(accessError, want) || !errors.Is(refreshError, want) {
			t.Fatal("撤销后访问或刷新校验结果错误", accessError, refreshError)
		}
	}
	type snapshot struct {
		hash                            []byte
		generation                      uint64
		createdAt, expiresAt, updatedAt time.Time
		lastSeenAt, revokedAt           sql.NullTime
	}
	read := func(t *testing.T, sessionID uint64) snapshot {
		t.Helper()
		var value snapshot
		err := pool.QueryRowContext(ctx, "SELECT refresh_token_hash,token_generation,created_at,expires_at,updated_at,last_seen_at,revoked_at FROM auth_sessions WHERE id=?", sessionID).
			Scan(&value.hash, &value.generation, &value.createdAt, &value.expiresAt, &value.updatedAt, &value.lastSeenAt, &value.revokedAt)
		if err != nil {
			t.Fatal("读取会话快照失败", err)
		}
		return value
	}
	revoke := func(actor domain.Actor, targetID uint64) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return txStore.RevokeUserDevice(txContext, targetID)
		})
	}
	type deviceSnapshot struct {
		installation, name, platform string
		createdAt, updatedAt         time.Time
		lastSeenAt, revokedAt        sql.NullTime
	}
	readDevice := func(t *testing.T, deviceID uint64) deviceSnapshot {
		t.Helper()
		var value deviceSnapshot
		if err := pool.QueryRowContext(ctx, "SELECT installation_id,name,platform,created_at,updated_at,last_seen_at,revoked_at FROM user_devices WHERE id=?", deviceID).
			Scan(&value.installation, &value.name, &value.platform, &value.createdAt, &value.updatedAt, &value.lastSeenAt, &value.revokedAt); err != nil {
			t.Fatal("读取设备撤销快照失败", err)
		}
		return value
	}
	// 2. 同设备全部会话原子撤销，已撤销时间和其他设备事实按原记录保留。
	t.Run("device sessions scope retries and login recovery", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		installation := uuid.NewString()
		target := newFixture(t, 1, installation)
		otherSession := newFixture(t, 1, installation)
		oldSession := newFixture(t, 1, installation)
		expiredSession := newFixture(t, 1, installation)
		foreign := newFixture(t, 3, installation)
		oldTime := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=?,updated_at=? WHERE id=?", oldTime, oldTime, oldSession.actor.SessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=? WHERE id=?", oldTime, expiredSession.actor.SessionID); err != nil {
			t.Fatal(err)
		}
		deviceBefore := readDevice(t, target.actor.DeviceID)
		callerBefore, foreignBefore := read(t, caller.actor.SessionID), read(t, foreign.actor.SessionID)
		targets := []fixture{target, otherSession, oldSession, expiredSession}
		before := make([]snapshot, len(targets))
		for index, current := range targets {
			before[index] = read(t, current.actor.SessionID)
		}
		if err := revoke(caller.actor, target.actor.DeviceID); err != nil {
			t.Fatal("撤销设备失败", err)
		}
		deviceAfter := readDevice(t, target.actor.DeviceID)
		expectedDevice := deviceBefore
		expectedDevice.revokedAt, expectedDevice.updatedAt = deviceAfter.revokedAt, deviceAfter.updatedAt
		if !deviceAfter.revokedAt.Valid || !deviceAfter.updatedAt.Equal(deviceAfter.revokedAt.Time) || deviceAfter.updatedAt.Nanosecond()%1000 != 0 || !reflect.DeepEqual(deviceAfter, expectedDevice) {
			t.Fatal("设备撤销时间或字段范围错误")
		}
		for index, current := range targets {
			assertCredentials(t, current, domain.ErrSessionRevoked)
			after := read(t, current.actor.SessionID)
			expected := before[index]
			if !expected.revokedAt.Valid {
				expected.revokedAt, expected.updatedAt = after.revokedAt, after.updatedAt
				if !after.revokedAt.Valid || !after.updatedAt.Equal(after.revokedAt.Time) || after.updatedAt.Nanosecond()%1000 != 0 {
					t.Fatal("关联会话撤销时间错误")
				}
			}
			if !reflect.DeepEqual(after, expected) {
				t.Fatal("会话历史或凭证字段被改写")
			}
			before[index] = after
		}
		if err := revoke(caller.actor, target.actor.DeviceID); err != nil || !reflect.DeepEqual(readDevice(t, target.actor.DeviceID), deviceAfter) {
			t.Fatal("重复设备撤销覆盖首次时间", err)
		}
		for index, current := range targets {
			if !reflect.DeepEqual(read(t, current.actor.SessionID), before[index]) {
				t.Fatal("重复设备撤销覆盖会话首次时间")
			}
		}
		for _, other := range []fixture{caller, foreign} {
			assertCredentials(t, other, nil)
		}
		if !reflect.DeepEqual(read(t, caller.actor.SessionID), callerBefore) || !reflect.DeepEqual(read(t, foreign.actor.SessionID), foreignBefore) {
			t.Fatal("撤销影响其他设备的会话")
		}
		for _, targetID := range []uint64{foreign.actor.DeviceID, ^uint64(0)} {
			if err := revoke(caller.actor, targetID); !errors.Is(err, domain.ErrDeviceNotFound) {
				t.Fatal("跨账号或缺失设备未统一处理", err)
			}
		}
		restored := newFixture(t, 1, installation)
		if restored.actor.DeviceID != target.actor.DeviceID || restored.actor.SessionID == target.actor.SessionID || readDevice(t, restored.actor.DeviceID).revokedAt.Valid {
			t.Fatal("重新登录未复用并恢复设备")
		}
		assertCredentials(t, restored, nil)
		for _, current := range targets {
			assertCredentials(t, current, domain.ErrSessionRevoked)
		}
	})
	t.Run("current device invalidates all its sessions", func(t *testing.T) {
		installation := uuid.NewString()
		caller, sameDevice := newFixture(t, 1, installation), newFixture(t, 1, installation)
		observer := newFixture(t, 1, uuid.NewString())
		if err := revoke(caller.actor, caller.actor.DeviceID); err != nil {
			t.Fatal(err)
		}
		assertCredentials(t, caller, domain.ErrSessionRevoked)
		assertCredentials(t, sameDevice, domain.ErrSessionRevoked)
		assertCredentials(t, observer, nil)
		if err := revoke(caller.actor, caller.actor.DeviceID); !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("失效调用者未被事务入口拒绝", err)
		}
		if err := revoke(observer.actor, caller.actor.DeviceID); err != nil {
			t.Fatal("有效观察设备重复撤销失败", err)
		}
	})
	t.Run("already revoked device repairs live sessions and supports empty device", func(t *testing.T) {
		caller, target := newFixture(t, 1, uuid.NewString()), newFixture(t, 1, uuid.NewString())
		oldTime := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=?,updated_at=? WHERE id=?", oldTime, oldTime, target.actor.DeviceID); err != nil {
			t.Fatal(err)
		}
		before := readDevice(t, target.actor.DeviceID)
		if err := revoke(caller.actor, target.actor.DeviceID); err != nil || !reflect.DeepEqual(readDevice(t, target.actor.DeviceID), before) {
			t.Fatal("补齐会话时覆盖设备首次时间", err)
		}
		assertCredentials(t, target, domain.ErrSessionRevoked)
		var empty *domain.LoginDevice
		if err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			var err error
			empty, err = txStore.CreateLoginDevice(txContext, uuid.NewString(), "无会话历史设备", "ANDROID")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := revoke(caller.actor, empty.ID); err != nil || !readDevice(t, empty.ID).revokedAt.Valid {
			t.Fatal("无会话设备撤销失败", err)
		}
	})
	// 3. 回调故障与关联会话写入等待均使两张表一起回滚。
	t.Run("error panic and cancellation roll back both tables", func(t *testing.T) {
		caller, target := newFixture(t, 1, uuid.NewString()), newFixture(t, 1, uuid.NewString())
		deviceBefore, sessionBefore := readDevice(t, target.actor.DeviceID), read(t, target.actor.SessionID)
		fault := errors.New("device revoke callback fault")
		for _, mode := range []string{"error", "panic", "cancel"} {
			t.Run(mode, func(t *testing.T) {
				requestContext, stop := context.WithCancel(ctx)
				defer stop()
				var recovered any
				var operationError error
				func() {
					defer func() { recovered = recover() }()
					operationError = store.WithUserTransaction(requestContext, caller.actor, func(txContext context.Context, txStore *Store) error {
						if err := txStore.RevokeUserDevice(txContext, target.actor.DeviceID); err != nil {
							return err
						}
						switch mode {
						case "error":
							return fault
						case "panic":
							panic("device revoke panic")
						default:
							stop()
							return nil
						}
					})
				}()
				if (mode == "error" && !errors.Is(operationError, fault)) || (mode == "panic" && recovered == nil) || (mode == "cancel" && !errors.Is(operationError, context.Canceled)) ||
					!reflect.DeepEqual(readDevice(t, target.actor.DeviceID), deviceBefore) || !reflect.DeepEqual(read(t, target.actor.SessionID), sessionBefore) {
					t.Fatal("设备及会话回滚或错误原因不正确", operationError)
				}
			})
		}
	})
	for _, locked := range []string{"foreign device", "owned device", "owned session"} {
		t.Run(locked+" lock", func(t *testing.T) {
			caller := newFixture(t, 1, uuid.NewString())
			owner := uint64(1)
			if locked == "foreign device" {
				owner = 3
			}
			target := newFixture(t, owner, uuid.NewString())
			deviceBefore, sessionBefore := readDevice(t, target.actor.DeviceID), read(t, target.actor.SessionID)
			holding, err := pool.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer holding.Rollback()
			statement, identifier := "SELECT id FROM user_devices WHERE id=? FOR UPDATE", target.actor.DeviceID
			if locked == "owned session" {
				statement, identifier = "SELECT id FROM auth_sessions WHERE id=? FOR UPDATE", target.actor.SessionID
			}
			var lockedID uint64
			if err := holding.QueryRowContext(ctx, statement, identifier).Scan(&lockedID); err != nil {
				t.Fatal(err)
			}
			requestContext, stop := context.WithTimeout(ctx, 200*time.Millisecond)
			defer stop()
			err = store.WithUserTransaction(requestContext, caller.actor, func(txContext context.Context, txStore *Store) error {
				return txStore.RevokeUserDevice(txContext, target.actor.DeviceID)
			})
			want := error(context.DeadlineExceeded)
			if locked == "foreign device" {
				want = domain.ErrDeviceNotFound
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(readDevice(t, target.actor.DeviceID), deviceBefore) || !reflect.DeepEqual(read(t, target.actor.SessionID), sessionBefore) {
				t.Fatal("归属、锁等待或两表回滚错误", err)
			}
		})
	}
	// 4. 并发重复撤销等待同一用户锁，并保留首次提交的时间。
	t.Run("concurrent revocation preserves first timestamps", func(t *testing.T) {
		caller, other := newFixture(t, 1, uuid.NewString()), newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		updated, releaseFirst := make(chan struct{}), make(chan struct{})
		firstResult, secondResult := make(chan error, 1), make(chan error, 1)
		var once sync.Once
		release := func() { once.Do(func() { close(releaseFirst) }) }
		defer release()
		var firstTime time.Time
		go func() {
			firstResult <- store.WithUserTransaction(ctx, caller.actor, func(txContext context.Context, txStore *Store) error {
				if err := txStore.RevokeUserDevice(txContext, target.actor.DeviceID); err != nil {
					return err
				}
				if err := txStore.database.WithContext(txContext).Raw("SELECT revoked_at FROM user_devices WHERE id=?", target.actor.DeviceID).Row().Scan(&firstTime); err != nil {
					return err
				}
				close(updated)
				<-releaseFirst
				return nil
			})
		}()
		select {
		case <-updated:
		case <-time.After(2 * time.Second):
			t.Fatal("首次设备撤销未到等待点")
		}
		go func() { secondResult <- revoke(other.actor, target.actor.DeviceID) }()
		select {
		case err := <-secondResult:
			t.Fatal("首次事务提交前第二次撤销已结束", err)
		case <-time.After(80 * time.Millisecond):
		}
		release()
		for _, result := range []chan error{firstResult, secondResult} {
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("并发设备撤销未结束")
			}
		}
		if !readDevice(t, target.actor.DeviceID).revokedAt.Time.Equal(firstTime) {
			t.Fatal("并发撤销覆盖设备首次时间")
		}
		assertCredentials(t, target, domain.ErrSessionRevoked)
		assertCredentials(t, caller, nil)
		assertCredentials(t, other, nil)
	})
	t.Run("full unsigned target device", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,1,?,'完整 ID 设备','ANDROID')", maximum, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if err := revoke(caller.actor, maximum); err != nil || !readDevice(t, maximum).revokedAt.Valid {
			t.Fatal("完整 uint64 设备撤销失败", err)
		}
	})
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("设备撤销操作关闭了共享连接池", err)
	}
}
