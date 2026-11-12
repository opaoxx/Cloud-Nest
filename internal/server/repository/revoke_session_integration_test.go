package repository

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRevokeCurrentSessionIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
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
	withRefresh := func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
		return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
			return action(txContext, txStore, current)
		})
	}
	refreshSessions := service.NewRefreshSessionService(credentials.Verify, withRefresh)
	refreshTokens := service.NewRefreshTokenService(refreshSessions.Refresh, signer.Issue)
	type fixture struct {
		actor       domain.Actor
		access, raw string
		session     domain.LoginSession
	}
	newFixture := func(t *testing.T, userID uint64, installationID string) fixture {
		t.Helper()
		raw, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installationID, "撤销测试设备", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		token, err := signer.Issue(*session)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("夹具授权失败", err)
		}
		return fixture{actor: *actor, access: token.Token, raw: raw, session: *session}
	}
	assertUsable := func(t *testing.T, current fixture) {
		t.Helper()
		actor, err := authorization.Authorize(ctx, current.access)
		if err != nil || actor == nil || *actor != current.actor {
			t.Fatal("有效访问凭证授权异常", err)
		}
		verified, err := credentials.Verify(ctx, current.raw)
		if err != nil || verified == nil || verified.Session.ID != current.actor.SessionID {
			t.Fatal("有效刷新凭证异常", err)
		}
	}
	assertRevoked := func(t *testing.T, current fixture) {
		t.Helper()
		if actor, err := authorization.Authorize(ctx, current.access); actor != nil || !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("撤销后访问授权仍有效", err)
		}
		if result, err := refreshTokens.Refresh(ctx, current.raw); result != nil || !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("撤销后仍可刷新", err)
		}
	}
	revoke := func(current fixture) error {
		return store.WithUserTransaction(ctx, current.actor, func(txContext context.Context, txStore *Store) error { return txStore.RevokeCurrentSession(txContext) })
	}
	t.Run("commit and current session scope", func(t *testing.T) {
		installation := uuid.NewString()
		current := newFixture(t, 1, installation)
		sameDevice := newFixture(t, 1, installation)
		otherDevice := newFixture(t, 1, uuid.NewString())
		otherUser := newFixture(t, 3, uuid.NewString())
		if err := revoke(current); err != nil {
			t.Fatal("提交撤销失败", err)
		}
		assertRevoked(t, current)
		assertUsable(t, sameDevice)
		assertUsable(t, otherDevice)
		assertUsable(t, otherUser)
		var revokedAt, updatedAt, expiresAt time.Time
		var generation uint64
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at,updated_at,expires_at,token_generation FROM auth_sessions WHERE id=?", current.actor.SessionID).Scan(&revokedAt, &updatedAt, &expiresAt, &generation); err != nil {
			t.Fatal(err)
		}
		if revokedAt.IsZero() || !updatedAt.Equal(revokedAt) || !expiresAt.Equal(current.session.ExpiresAt) || generation != 1 {
			t.Fatal("撤销时间或会话资料不符合预期")
		}
	})
	t.Run("error panic cancellation and repeat roll back", func(t *testing.T) {
		current := newFixture(t, 1, uuid.NewString())
		failure := errors.New("fixture callback failure")
		for _, mode := range []string{"error", "panic", "cancel", "repeat"} {
			t.Run(mode, func(t *testing.T) {
				requestContext, cancelRequest := context.WithCancel(ctx)
				defer cancelRequest()
				action := func(txContext context.Context, txStore *Store) error {
					if err := txStore.RevokeCurrentSession(txContext); err != nil {
						return err
					}
					switch mode {
					case "panic":
						panic(failure)
					case "cancel":
						cancelRequest()
						return nil
					case "repeat":
						return txStore.RevokeCurrentSession(txContext)
					default:
						return failure
					}
				}
				if mode == "panic" {
					func() {
						defer func() {
							if recover() != failure {
								t.Error("panic 未按原值传播")
							}
						}()
						_ = store.WithUserTransaction(requestContext, current.actor, action)
					}()
				} else {
					err := store.WithUserTransaction(requestContext, current.actor, action)
					want := failure
					if mode == "cancel" {
						want = context.Canceled
					}
					if mode == "repeat" {
						want = domain.ErrSessionRevoked
					}
					if !errors.Is(err, want) {
						t.Fatal("事务失败原因错误", err)
					}
				}
				assertUsable(t, current)
			})
		}
	})
	t.Run("already revoked rejected by transaction entry", func(t *testing.T) {
		current := newFixture(t, 1, uuid.NewString())
		if err := revoke(current); err != nil {
			t.Fatal(err)
		}
		called := false
		err := store.WithUserTransaction(ctx, current.actor, func(txContext context.Context, txStore *Store) error {
			called = true
			return txStore.RevokeCurrentSession(txContext)
		})
		if !errors.Is(err, domain.ErrSessionRevoked) || called {
			t.Fatal("已撤销会话进入写回调", err)
		}
	})
	t.Run("revoke after a successful refresh", func(t *testing.T) {
		current := newFixture(t, 1, uuid.NewString())
		refreshed, err := refreshTokens.Refresh(ctx, current.raw)
		if err != nil || refreshed == nil {
			t.Fatal(err)
		}
		if err := revoke(current); err != nil {
			t.Fatal(err)
		}
		current.access, current.raw = refreshed.AccessToken.Token, refreshed.RefreshToken
		assertRevoked(t, current)
	})
	t.Run("refresh waits for committed revocation", func(t *testing.T) {
		current := newFixture(t, 1, uuid.NewString())
		written, release := make(chan struct{}), make(chan struct{})
		firstDone := make(chan error, 1)
		var once sync.Once
		releaseRevoke := func() { once.Do(func() { close(release) }) }
		defer releaseRevoke()
		go func() {
			firstDone <- store.WithUserTransaction(ctx, current.actor, func(txContext context.Context, txStore *Store) error {
				if err := txStore.RevokeCurrentSession(txContext); err != nil {
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
			t.Fatal("撤销未在事务中执行")
		}
		verified := make(chan struct{})
		waitingSessions := service.NewRefreshSessionService(func(ctx context.Context, raw string) (*domain.VerifiedRefreshSession, error) {
			result, err := credentials.Verify(ctx, raw)
			if err == nil {
				close(verified)
			}
			return result, err
		}, withRefresh)
		waitingTokens := service.NewRefreshTokenService(waitingSessions.Refresh, signer.Issue)
		refreshDone := make(chan error, 1)
		go func() {
			result, err := waitingTokens.Refresh(ctx, current.raw)
			if result != nil {
				refreshDone <- errors.New("等待中的刷新返回了凭证")
				return
			}
			refreshDone <- err
		}()
		select {
		case <-verified:
		case err := <-refreshDone:
			t.Fatal("刷新未完成初次快照校验", err)
		case <-time.After(2 * time.Second):
			t.Fatal("初次刷新校验未结束")
		}
		select {
		case err := <-refreshDone:
			t.Fatal("撤销提交前刷新已结束", err)
		case <-time.After(80 * time.Millisecond):
		}
		releaseRevoke()
		select {
		case err := <-firstDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("撤销事务未结束")
		}
		select {
		case err := <-refreshDone:
			if !errors.Is(err, domain.ErrSessionRevoked) {
				t.Fatal("等待后刷新未发现撤销", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("等待刷新未结束")
		}
		assertRevoked(t, current)
	})
	t.Run("maximum unsigned current identity", func(t *testing.T) {
		maximum := ^uint64(0)
		raw, hash := service.GenerateRefreshToken()
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,status) VALUES(?,'ACTIVE')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,?,'最大标识','WEB')", maximum, maximum, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		session := domain.LoginSession{ID: maximum, UserID: maximum, DeviceID: maximum, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,?,?)", maximum, maximum, maximum, hash[:], session.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		token, err := signer.Issue(session)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("最大身份授权失败", err)
		}
		current := fixture{actor: *actor, access: token.Token, raw: raw, session: session}
		if err := revoke(current); err != nil {
			t.Fatal("最大身份撤销失败", err)
		}
		assertRevoked(t, current)
	})
}
