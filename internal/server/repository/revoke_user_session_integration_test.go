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

func TestRevokeUserSessionIntegration(t *testing.T) {
	// 1. 在独占库中装配真实事务、JWT 授权和刷新凭证校验。
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
			device, err := txStore.EnsureLoginDevice(txContext, installation, "指定会话撤销测试", "WEB")
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
			return txStore.RevokeUserSession(txContext, targetID)
		})
	}

	// 2. 验证目标范围、历史状态、重复调用和失败回滚。
	t.Run("scope and idempotent timestamp", func(t *testing.T) {
		installation := uuid.NewString()
		caller := newFixture(t, 1, installation)
		sameDevice := newFixture(t, 1, installation)
		target := newFixture(t, 1, uuid.NewString())
		foreign := newFixture(t, 3, uuid.NewString())
		original := read(t, target.actor.SessionID)
		others := []fixture{caller, sameDevice, foreign}
		baselines := make([]snapshot, len(others))
		for index, other := range others {
			baselines[index] = read(t, other.actor.SessionID)
		}
		if err := revoke(caller.actor, target.actor.SessionID); err != nil {
			t.Fatal("撤销其他设备会话失败", err)
		}
		assertCredentials(t, target, domain.ErrSessionRevoked)
		committed := read(t, target.actor.SessionID)
		if !committed.revokedAt.Valid || !committed.updatedAt.Equal(committed.revokedAt.Time) || committed.revokedAt.Time.Nanosecond()%1000 != 0 {
			t.Fatal("数据库撤销时间错误")
		}
		expected := original
		expected.revokedAt, expected.updatedAt = committed.revokedAt, committed.updatedAt
		if !reflect.DeepEqual(committed, expected) {
			t.Fatal("目标会话的其他字段被修改")
		}
		for attempt := 0; attempt < 2; attempt++ {
			if err := revoke(caller.actor, target.actor.SessionID); err != nil || !reflect.DeepEqual(read(t, target.actor.SessionID), committed) {
				t.Fatal("重复撤销未成功或覆盖首次撤销时间", err)
			}
		}
		for _, targetID := range []uint64{foreign.actor.SessionID, ^uint64(0)} {
			if err := revoke(caller.actor, targetID); !errors.Is(err, domain.ErrSessionNotFound) {
				t.Fatal("其他账号与不存在的目标未统一处理", err)
			}
		}
		for index, other := range others {
			assertCredentials(t, other, nil)
			if !reflect.DeepEqual(read(t, other.actor.SessionID), baselines[index]) {
				t.Fatal("其他会话被修改")
			}
		}
		if err := revoke(caller.actor, sameDevice.actor.SessionID); err != nil {
			t.Fatal("撤销同设备另一会话失败", err)
		}
		assertCredentials(t, sameDevice, domain.ErrSessionRevoked)
		assertCredentials(t, caller, nil)
		var revokedDevices int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_devices WHERE revoked_at IS NOT NULL").Scan(&revokedDevices); err != nil || revokedDevices != 0 {
			t.Fatal("会话撤销影响了设备状态", err)
		}
	})
	t.Run("error panic and cancellation rollback", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		baseline := read(t, target.actor.SessionID)
		failure := errors.New("fixture rollback")
		for _, mode := range []string{"error", "panic", "cancel"} {
			t.Run(mode, func(t *testing.T) {
				requestContext, cancelRequest := context.WithCancel(ctx)
				defer cancelRequest()
				action := func(txContext context.Context, txStore *Store) error {
					if err := txStore.RevokeUserSession(txContext, target.actor.SessionID); err != nil {
						return err
					}
					switch mode {
					case "panic":
						panic(failure)
					case "cancel":
						cancelRequest()
						return nil
					default:
						return failure
					}
				}
				if mode == "panic" {
					func() {
						defer func() {
							if recover() != failure {
								t.Error("原 panic 未传播")
							}
						}()
						_ = store.WithUserTransaction(requestContext, caller.actor, action)
					}()
				} else {
					want := failure
					if mode == "cancel" {
						want = context.Canceled
					}
					if err := store.WithUserTransaction(requestContext, caller.actor, action); !errors.Is(err, want) {
						t.Fatal("回滚原因错误", err)
					}
				}
				if !reflect.DeepEqual(read(t, target.actor.SessionID), baseline) {
					t.Fatal("事务失败后撤销仍被保存")
				}
				assertCredentials(t, caller, nil)
				assertCredentials(t, target, nil)
			})
		}
	})
	t.Run("historical expired and device revoked targets", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		for _, state := range []string{"expired", "device revoked"} {
			target := newFixture(t, 1, uuid.NewString())
			if state == "expired" {
				_, err = pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=?", target.actor.SessionID)
			} else {
				_, err = pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", target.actor.DeviceID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := revoke(caller.actor, target.actor.SessionID); err != nil {
				t.Fatal("历史目标撤销失败", err)
			}
			assertCredentials(t, target, domain.ErrSessionRevoked)
		}
		assertCredentials(t, caller, nil)
	})
	t.Run("concurrent revocation preserves first timestamp", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		otherCaller := newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		written, release := make(chan struct{}), make(chan struct{})
		firstDone, secondDone := make(chan error, 1), make(chan error, 1)
		var once sync.Once
		releaseFirst := func() { once.Do(func() { close(release) }) }
		defer releaseFirst()
		var firstTimestamp time.Time
		go func() {
			firstDone <- store.WithUserTransaction(ctx, caller.actor, func(txContext context.Context, txStore *Store) error {
				if err := txStore.RevokeUserSession(txContext, target.actor.SessionID); err != nil {
					return err
				}
				if err := txStore.database.WithContext(txContext).Raw("SELECT revoked_at FROM auth_sessions WHERE id=?", target.actor.SessionID).Row().Scan(&firstTimestamp); err != nil {
					return err
				}
				close(written)
				select {
				case <-release:
					return nil
				case <-txContext.Done():
					return txContext.Err()
				}
			})
		}()
		select {
		case <-written:
		case <-time.After(2 * time.Second):
			t.Fatal("首个撤销未执行")
		}
		go func() { secondDone <- revoke(otherCaller.actor, target.actor.SessionID) }()
		select {
		case err := <-secondDone:
			t.Fatal("首个事务持有用户锁时第二次撤销已结束", err)
		case <-time.After(50 * time.Millisecond):
		}
		releaseFirst()
		for _, done := range []chan error{firstDone, secondDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("并发撤销失败", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("并发撤销未结束")
			}
		}
		committed := read(t, target.actor.SessionID)
		if !committed.revokedAt.Valid || !committed.revokedAt.Time.Equal(firstTimestamp) || !committed.updatedAt.Equal(firstTimestamp) {
			t.Fatal("并发重复撤销覆盖首次时间")
		}
		assertCredentials(t, caller, nil)
		assertCredentials(t, otherCaller, nil)
		assertCredentials(t, target, domain.ErrSessionRevoked)
	})
	t.Run("foreign locked target returns not found", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		foreign := newFixture(t, 3, uuid.NewString())
		holding, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer holding.Rollback()
		var lockedID uint64
		if err := holding.QueryRowContext(ctx, "SELECT id FROM auth_sessions WHERE id=? FOR UPDATE", foreign.actor.SessionID).Scan(&lockedID); err != nil {
			t.Fatal(err)
		}
		requestContext, cancelRequest := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancelRequest()
		err = store.WithUserTransaction(requestContext, caller.actor, func(txContext context.Context, txStore *Store) error {
			return txStore.RevokeUserSession(txContext, foreign.actor.SessionID)
		})
		if !errors.Is(err, domain.ErrSessionNotFound) {
			t.Fatal("其他账号目标被加锁时未直接返回不存在", err)
		}
	})
	t.Run("owned locked target honors deadline", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		baseline := read(t, target.actor.SessionID)
		holding, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer holding.Rollback()
		var lockedID uint64
		if err := holding.QueryRowContext(ctx, "SELECT id FROM auth_sessions WHERE id=? FOR UPDATE", target.actor.SessionID).Scan(&lockedID); err != nil {
			t.Fatal(err)
		}
		requestContext, cancelRequest := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancelRequest()
		err = store.WithUserTransaction(requestContext, caller.actor, func(txContext context.Context, txStore *Store) error {
			return txStore.RevokeUserSession(txContext, target.actor.SessionID)
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("本人目标行锁等待未服从请求期限", err)
		}
		if err := holding.Rollback(); err != nil {
			t.Fatal("释放测试行锁失败", err)
		}
		if !reflect.DeepEqual(read(t, target.actor.SessionID), baseline) {
			t.Fatal("锁等待取消后目标被修改")
		}
		assertCredentials(t, caller, nil)
		assertCredentials(t, target, nil)
	})
	t.Run("current target and stale actor", func(t *testing.T) {
		caller := newFixture(t, 1, uuid.NewString())
		target := newFixture(t, 1, uuid.NewString())
		if err := revoke(caller.actor, caller.actor.SessionID); err != nil {
			t.Fatal("指定自身会话撤销失败", err)
		}
		assertCredentials(t, caller, domain.ErrSessionRevoked)
		called := false
		err := store.WithUserTransaction(ctx, caller.actor, func(txContext context.Context, txStore *Store) error {
			called = true
			return txStore.RevokeUserSession(txContext, target.actor.SessionID)
		})
		if called || !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("失效调用者进入写回调", err)
		}
		assertCredentials(t, target, nil)
	})
	// 3. 最后使用完整 uint64 标识，避免最大自增值影响其他夹具。
	t.Run("maximum unsigned owner device and target", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,status) VALUES(?,'ACTIVE')", maximum); err != nil {
			t.Fatal(err)
		}
		fixtures := make([]fixture, 0, 2)
		for _, identifier := range []uint64{maximum - 1, maximum} {
			if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,?,'最大标识','WEB')", identifier, maximum, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			refresh, hash := service.GenerateRefreshToken()
			session := domain.LoginSession{ID: identifier, UserID: maximum, DeviceID: identifier, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
			if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,?,?)", session.ID, session.UserID, session.DeviceID, hash[:], session.ExpiresAt); err != nil {
				t.Fatal(err)
			}
			fixtures = append(fixtures, issueFixture(t, session, refresh))
		}
		if err := revoke(fixtures[0].actor, maximum); err != nil {
			t.Fatal("最大标识目标撤销失败", err)
		}
		assertCredentials(t, fixtures[0], nil)
		assertCredentials(t, fixtures[1], domain.ErrSessionRevoked)
	})
}
