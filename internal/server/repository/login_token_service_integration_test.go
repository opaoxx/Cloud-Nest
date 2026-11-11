package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestLoginTokenServiceIntegration(t *testing.T) {
	// 1. 复用独占设备、会话库，连接真实事务服务和 JWT 组件。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.JWTConfig{
		SigningKey: bytes.Repeat([]byte{7}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 30 * 24 * time.Hour,
	}
	sessions, err := service.NewLoginSessionService(cfg, func(ctx context.Context, userID uint64, action func(context.Context, service.LoginSessionTransaction) error) error {
		return store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := service.NewAccessTokenSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	creator := service.NewLoginTokenService(sessions.Create, signer.Issue)
	result, err := creator.Create(ctx, domain.VerifiedAccount{ID: 1}, uuid.NewString(), "凭证联测设备", "WEB")
	if err != nil || result == nil {
		t.Fatal("真实会话与凭证组合失败")
	}

	// 2. 校验 JWT 标识与数据库摘要，再通过真实授权查询取得 Actor。
	identity, err := verifier.Verify(result.AccessToken.Token)
	if err != nil || identity == nil || identity.UserID != 1 || identity.SessionID != result.Session.ID ||
		!identity.ExpiresAt.Equal(result.AccessToken.ExpiresAt) {
		t.Fatal("实际签发 JWT 与已提交会话不一致")
	}
	var storedHash []byte
	var storedDeviceID uint64
	if err := pool.QueryRowContext(ctx, "SELECT refresh_token_hash,device_id FROM auth_sessions WHERE user_id=1 AND id=?", result.Session.ID).Scan(&storedHash, &storedDeviceID); err != nil {
		t.Fatal("提交会话读回失败")
	}
	expectedHash := sha256.Sum256([]byte(result.RefreshToken))
	if !bytes.Equal(storedHash, expectedHash[:]) || storedDeviceID != result.Device.ID {
		t.Fatal("刷新凭证摘要或设备归属不一致")
	}
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	actor, err := authorization.Authorize(ctx, result.AccessToken.Token)
	if err != nil || actor == nil || actor.UserID != 1 || actor.SessionID != result.Session.ID || actor.DeviceID != result.Device.ID {
		t.Fatal("组合结果未通过真实数据库授权")
	}

	// 3. 提交后的签发故障或取消返回空凭证结果，单独查询核对已提交状态。
	for _, afterCommitCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "signing fault after commit", true: "cancellation after commit"}[afterCommitCancel], func(t *testing.T) {
			requestContext, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			var committed *domain.LoginSessionResult
			fault := errors.New("fixture signing failure")
			failing := service.NewLoginTokenService(func(ctx context.Context, account domain.VerifiedAccount, installationID, name, platform string) (*domain.LoginSessionResult, error) {
				var err error
				committed, err = sessions.Create(ctx, account, installationID, name, platform)
				if err == nil && afterCommitCancel {
					cancelRequest()
				}
				return committed, err
			}, func(domain.LoginSession) (*domain.AccessToken, error) {
				if afterCommitCancel {
					t.Error("提交后取消仍执行了签发")
				}
				return nil, fault
			})
			result, err := failing.Create(requestContext, domain.VerifiedAccount{ID: 1}, uuid.NewString(), "提交边界设备", "WEB")
			expectedError := fault
			if afterCommitCancel {
				expectedError = context.Canceled
			}
			if result != nil || !errors.Is(err, expectedError) || committed == nil {
				t.Fatal("提交后的错误原因或结果边界异常")
			}
			var rows int
			if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM auth_sessions WHERE user_id=1 AND id=? AND device_id=?", committed.Session.ID, committed.Device.ID).Scan(&rows); err != nil || rows != 1 {
				t.Fatal("已提交状态丢失")
			}
		})
	}
}
