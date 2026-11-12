package repository

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestLogoutServiceIntegration(t *testing.T) {
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
	withTransaction := func(ctx context.Context, actor domain.Actor, action func(context.Context, service.LogoutTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	}
	logout := service.NewLogoutService(withTransaction)
	type fixture struct {
		actor       domain.Actor
		access, raw string
	}
	newFixture := func(t *testing.T, installation string) fixture {
		t.Helper()
		raw, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installation, "退出服务测试设备", "WEB")
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
		return fixture{actor: *actor, access: token.Token, raw: raw}
	}
	assertState := func(t *testing.T, current fixture, revoked bool) {
		t.Helper()
		actor, accessError := authorization.Authorize(ctx, current.access)
		verified, refreshError := credentials.Verify(ctx, current.raw)
		if revoked {
			if actor != nil || verified != nil || !errors.Is(accessError, domain.ErrSessionRevoked) || !errors.Is(refreshError, domain.ErrSessionRevoked) {
				t.Fatal("已提交退出未使凭证失效", accessError, refreshError)
			}
		} else if accessError != nil || refreshError != nil || actor == nil || verified == nil || *actor != current.actor {
			t.Fatal("回滚后凭证或独立会话异常", accessError, refreshError)
		}
	}
	t.Run("committed result and session scope", func(t *testing.T) {
		installation := uuid.NewString()
		current, another := newFixture(t, installation), newFixture(t, installation)
		result, err := logout.Logout(ctx, current.actor)
		if err != nil || result == nil || !result.Revoked {
			t.Fatal("完整退出服务失败", err)
		}
		assertState(t, current, true)
		assertState(t, another, false)
	})
	t.Run("failure after callback rolls back", func(t *testing.T) {
		current := newFixture(t, uuid.NewString())
		failure := errors.New("fixture failure before commit")
		failing := service.NewLogoutService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.LogoutTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				if err := action(txContext, txStore); err != nil {
					return err
				}
				return failure
			})
		})
		result, err := failing.Logout(ctx, current.actor)
		if result != nil || !errors.Is(err, failure) {
			t.Fatal("失败事务返回了成功结果", err)
		}
		assertState(t, current, false)
	})
	t.Run("cancel before commit rolls back", func(t *testing.T) {
		current := newFixture(t, uuid.NewString())
		requestContext, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		cancelling := service.NewLogoutService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.LogoutTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				if err := action(txContext, txStore); err != nil {
					return err
				}
				cancelRequest()
				return nil
			})
		})
		result, err := cancelling.Logout(requestContext, current.actor)
		if result != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("提交前取消仍返回成功", err)
		}
		assertState(t, current, false)
	})
	t.Run("cancel after confirmed commit returns committed result", func(t *testing.T) {
		current := newFixture(t, uuid.NewString())
		requestContext, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		confirmed := service.NewLogoutService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.LogoutTransaction) error) error {
			err := withTransaction(ctx, actor, action)
			cancelRequest()
			return err
		})
		result, err := confirmed.Logout(requestContext, current.actor)
		if err != nil || result == nil || !result.Revoked || requestContext.Err() == nil {
			t.Fatal("已确认提交的撤销结果丢失", err)
		}
		assertState(t, current, true)
	})
	t.Run("revoked and mismatched identity fail", func(t *testing.T) {
		current := newFixture(t, uuid.NewString())
		mismatched := current.actor
		mismatched.UserID = 3
		result, err := logout.Logout(ctx, mismatched)
		if result != nil || !errors.Is(err, domain.ErrSessionNotFound) {
			t.Fatal("身份归属不匹配仍返回成功", err)
		}
		assertState(t, current, false)
		if _, err := logout.Logout(ctx, current.actor); err != nil {
			t.Fatal(err)
		}
		result, err = logout.Logout(ctx, current.actor)
		if result != nil || !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("已撤销会话的错误语义异常", err)
		}
	})
	t.Run("concurrent logout has one committed result", func(t *testing.T) {
		current := newFixture(t, uuid.NewString())
		type outcome struct {
			result *domain.LogoutResult
			err    error
		}
		results := make(chan outcome, 2)
		for range 2 {
			go func() { result, err := logout.Logout(ctx, current.actor); results <- outcome{result: result, err: err} }()
		}
		wins, rejections := 0, 0
		for range 2 {
			select {
			case received := <-results:
				if received.err == nil {
					wins++
					if received.result == nil || !received.result.Revoked {
						t.Fatal("并发成功结果异常")
					}
				} else {
					rejections++
					if received.result != nil || !errors.Is(received.err, domain.ErrSessionRevoked) {
						t.Fatal("并发失效结果异常", received.err)
					}
				}
			case <-time.After(6 * time.Second):
				t.Fatal("并发退出未结束")
			}
		}
		if wins != 1 || rejections != 1 {
			t.Fatal("并发退出未形成单次提交结果")
		}
		assertState(t, current, true)
	})
}
