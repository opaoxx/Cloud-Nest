package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRefreshTokenServiceIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{
		SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 30 * 24 * time.Hour,
	}
	signer, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	loginSessions, err := service.NewLoginSessionService(configuration, func(ctx context.Context, userID uint64, action func(context.Context, service.LoginSessionTransaction) error) error {
		return store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	})
	if err != nil {
		t.Fatal(err)
	}
	login := service.NewLoginTokenService(loginSessions.Create, signer.Issue)
	credentials := service.NewRefreshCredentialService(store.FindRefreshSession)
	sessions := service.NewRefreshSessionService(credentials.Verify, func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
		return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
			return action(txContext, txStore, current)
		})
	})
	refresher := service.NewRefreshTokenService(sessions.Refresh, signer.Issue)
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	createFixture := func(t *testing.T) *domain.LoginTokenResult {
		t.Helper()
		result, err := login.Create(ctx, domain.VerifiedAccount{ID: 1}, uuid.NewString(), "刷新凭证联测设备", "WEB")
		if err != nil || result == nil {
			t.Fatal("创建真实登录凭证失败", err)
		}
		return result
	}
	assertCommitted := func(t *testing.T, result *domain.RefreshSessionResult) {
		t.Helper()
		if result == nil {
			t.Fatal("缺少已提交刷新资料")
		}
		current, err := credentials.Verify(ctx, result.RefreshToken)
		if err != nil || current == nil || current.Session != result.Session || current.TokenGeneration != result.TokenGeneration || current.TokenHash != sha256.Sum256([]byte(result.RefreshToken)) {
			t.Fatal("新原文未对应已提交摘要、代次与会话", err)
		}
	}
	assertInvalid := func(t *testing.T, raw string) {
		t.Helper()
		if result, err := credentials.Verify(ctx, raw); result != nil || !errors.Is(err, domain.ErrInvalidRefreshToken) {
			t.Fatal("已轮换旧原文仍可用", err)
		}
	}
	assertAuthorized := func(t *testing.T, result *domain.RefreshTokenResult) {
		t.Helper()
		assertCommitted(t, &domain.RefreshSessionResult{Session: result.Session, TokenGeneration: result.TokenGeneration, RefreshToken: result.RefreshToken})
		identity, err := verifier.Verify(result.AccessToken.Token)
		if err != nil || identity == nil || identity.UserID != result.Session.UserID || identity.SessionID != result.Session.ID || !identity.ExpiresAt.Equal(result.AccessToken.ExpiresAt) {
			t.Fatal("刷新 JWT 签名或声明校验失败", err)
		}
		actor, err := authorization.Authorize(ctx, result.AccessToken.Token)
		if err != nil || actor == nil || actor.UserID != result.Session.UserID || actor.SessionID != result.Session.ID || actor.DeviceID != result.Session.DeviceID {
			t.Fatal("刷新 JWT 数据库授权失败", err)
		}
	}
	t.Run("two rotations with actual signing and authorization", func(t *testing.T) {
		created := createFixture(t)
		oldRaw := created.RefreshToken
		for generation := uint64(2); generation <= 3; generation++ {
			result, err := refresher.Refresh(ctx, oldRaw)
			if err != nil || result == nil || result.Session != created.Session || result.TokenGeneration != generation {
				t.Fatal("完整刷新凭证调用失败", err)
			}
			assertAuthorized(t, result)
			assertInvalid(t, oldRaw)
			oldRaw = result.RefreshToken
		}
		if actor, err := authorization.Authorize(ctx, created.AccessToken.Token); err != nil || actor == nil || actor.SessionID != created.Session.ID {
			t.Fatal("已有有效 JWT 的会话授权异常", err)
		}
	})
	t.Run("signing failure returns no result after confirmed commit", func(t *testing.T) {
		created := createFixture(t)
		fault := errors.New("fixture signing failure")
		var committed *domain.RefreshSessionResult
		failing := service.NewRefreshTokenService(func(ctx context.Context, raw string) (*domain.RefreshSessionResult, error) {
			var err error
			committed, err = sessions.Refresh(ctx, raw)
			return committed, err
		}, func(session domain.LoginSession) (*domain.AccessToken, error) {
			assertCommitted(t, committed)
			if session != created.Session {
				t.Error("签发未收到已提交会话")
			}
			return nil, fault
		})
		result, err := failing.Refresh(ctx, created.RefreshToken)
		if result != nil || !errors.Is(err, fault) {
			t.Fatal("签发失败返回了部分凭证结果", err)
		}
		assertInvalid(t, created.RefreshToken)
		assertCommitted(t, committed)
	})
	t.Run("cancellation after commit skips signing and preserves rotation", func(t *testing.T) {
		created := createFixture(t)
		requestContext, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		var committed *domain.RefreshSessionResult
		var signingCalls int
		cancelling := service.NewRefreshTokenService(func(ctx context.Context, raw string) (*domain.RefreshSessionResult, error) {
			var err error
			committed, err = sessions.Refresh(ctx, raw)
			cancelRequest()
			return committed, err
		}, func(session domain.LoginSession) (*domain.AccessToken, error) {
			signingCalls++
			return signer.Issue(session)
		})
		result, err := cancelling.Refresh(requestContext, created.RefreshToken)
		if result != nil || !errors.Is(err, context.Canceled) || signingCalls != 0 {
			t.Fatal("提交后取消仍签发或返回了凭证", err)
		}
		assertInvalid(t, created.RefreshToken)
		assertCommitted(t, committed)
	})
	t.Run("invalid original skips signing", func(t *testing.T) {
		var signingCalls int
		guarded := service.NewRefreshTokenService(sessions.Refresh, func(session domain.LoginSession) (*domain.AccessToken, error) {
			signingCalls++
			return signer.Issue(session)
		})
		result, err := guarded.Refresh(ctx, "bad-token")
		if result != nil || !errors.Is(err, domain.ErrInvalidRefreshToken) || signingCalls != 0 {
			t.Fatal("无效旧原文进入签发", err)
		}
	})
	t.Run("concurrent refresh signs only the committed winner", func(t *testing.T) {
		created := createFixture(t)
		var signingCalls atomic.Int32
		concurrent := service.NewRefreshTokenService(sessions.Refresh, func(session domain.LoginSession) (*domain.AccessToken, error) {
			signingCalls.Add(1)
			return signer.Issue(session)
		})
		type outcome struct {
			result *domain.RefreshTokenResult
			err    error
		}
		results := make(chan outcome, 2)
		for range 2 {
			go func() {
				result, err := concurrent.Refresh(ctx, created.RefreshToken)
				results <- outcome{result: result, err: err}
			}()
		}
		wins, rejections := 0, 0
		for range 2 {
			select {
			case received := <-results:
				if received.err == nil {
					wins++
					if received.result == nil || received.result.TokenGeneration != 2 {
						t.Fatal("胜者结果异常")
					}
					assertAuthorized(t, received.result)
				} else {
					rejections++
					if received.result != nil || !errors.Is(received.err, domain.ErrInvalidRefreshToken) {
						t.Fatal("竞争失败结果异常", received.err)
					}
				}
			case <-time.After(6 * time.Second):
				t.Fatal("并发刷新凭证未结束")
			}
		}
		if wins != 1 || rejections != 1 || signingCalls.Load() != 1 {
			t.Fatal("并发旧凭证未限制为一次提交和签发")
		}
	})
}
