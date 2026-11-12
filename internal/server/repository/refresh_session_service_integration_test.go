package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// conflictingRefreshTransaction 在真实更新前替换摘要，验证唯一键冲突后的回滚。
type conflictingRefreshTransaction struct {
	service.RefreshSessionTransaction
	hash [sha256.Size]byte
}

func (transaction *conflictingRefreshTransaction) RotateRefreshToken(ctx context.Context, _ [sha256.Size]byte) (*domain.VerifiedRefreshSession, error) {
	return transaction.RefreshSessionTransaction.RotateRefreshToken(ctx, transaction.hash)
}

func TestRefreshSessionServiceIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	credentials := service.NewRefreshCredentialService(store.FindRefreshSession)
	withTransaction := func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
		return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
			return action(txContext, txStore, current)
		})
	}
	refresher := service.NewRefreshSessionService(credentials.Verify, withTransaction)
	type fixture struct {
		raw      string
		verified domain.VerifiedRefreshSession
	}
	createFixture := func(t *testing.T) fixture {
		t.Helper()
		raw, hash := service.GenerateRefreshToken()
		err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "刷新服务测试设备", "WEB")
			if err != nil {
				return err
			}
			_, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		verified, err := credentials.Verify(ctx, raw)
		if err != nil || verified == nil {
			t.Fatal("建立刷新服务夹具失败", err)
		}
		return fixture{raw: raw, verified: *verified}
	}
	assertCurrent := func(t *testing.T, original fixture) {
		t.Helper()
		verified, err := credentials.Verify(ctx, original.raw)
		if err != nil || verified == nil || *verified != original.verified {
			t.Fatal("失败调用改变了已提交的旧凭证", err)
		}
	}
	t.Run("two successful refreshes", func(t *testing.T) {
		original := createFixture(t)
		for range 2 {
			result, err := refresher.Refresh(ctx, original.raw)
			if err != nil || result == nil || result.Session != original.verified.Session || result.TokenGeneration != original.verified.TokenGeneration+1 {
				t.Fatal("完整刷新调用失败", err)
			}
			if old, err := credentials.Verify(ctx, original.raw); old != nil || !errors.Is(err, domain.ErrInvalidRefreshToken) {
				t.Fatal("旧原文仍有效", err)
			}
			current, err := credentials.Verify(ctx, result.RefreshToken)
			if err != nil || current == nil || current.Session != result.Session || current.TokenGeneration != result.TokenGeneration || current.TokenHash != sha256.Sum256([]byte(result.RefreshToken)) {
				t.Fatal("新原文或提交资料不一致", err)
			}
			original = fixture{raw: result.RefreshToken, verified: *current}
		}
	})
	t.Run("invalid original fails before transaction", func(t *testing.T) {
		var called bool
		guarded := service.NewRefreshSessionService(credentials.Verify, func(context.Context, domain.VerifiedRefreshSession, func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
			called = true
			return nil
		})
		unknown, _ := service.GenerateRefreshToken()
		for _, raw := range []string{"bad-token", unknown} {
			result, err := guarded.Refresh(ctx, raw)
			if result != nil || !errors.Is(err, domain.ErrInvalidRefreshToken) || called {
				t.Fatal("无效原文进入了事务", err)
			}
		}
	})
	t.Run("failure after rotation rolls back", func(t *testing.T) {
		original := createFixture(t)
		failure := errors.New("fixture failure before commit")
		failing := service.NewRefreshSessionService(credentials.Verify, func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
			return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
				if err := action(txContext, txStore, current); err != nil {
					return err
				}
				return failure
			})
		})
		result, err := failing.Refresh(ctx, original.raw)
		if result != nil || !errors.Is(err, failure) {
			t.Fatal("回滚故障返回了凭证", err)
		}
		assertCurrent(t, original)
	})
	t.Run("cancel after rotation rolls back", func(t *testing.T) {
		original := createFixture(t)
		requestContext, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		cancelling := service.NewRefreshSessionService(credentials.Verify, func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
			return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
				if err := action(txContext, txStore, current); err != nil {
					return err
				}
				cancelRequest()
				return nil
			})
		})
		result, err := cancelling.Refresh(requestContext, original.raw)
		if result != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("取消的事务返回了凭证", err)
		}
		assertCurrent(t, original)
	})
	t.Run("digest collision rolls back", func(t *testing.T) {
		original, another := createFixture(t), createFixture(t)
		conflicting := service.NewRefreshSessionService(credentials.Verify, func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
			return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
				return action(txContext, &conflictingRefreshTransaction{RefreshSessionTransaction: txStore, hash: another.verified.TokenHash}, current)
			})
		})
		result, err := conflicting.Refresh(ctx, original.raw)
		if result != nil || !errors.Is(err, domain.ErrRefreshTokenConflict) {
			t.Fatal("真实摘要冲突未传播", err)
		}
		assertCurrent(t, original)
		assertCurrent(t, another)
	})
	for _, testCase := range []struct {
		name, sql string
		want      error
	}{
		{name: "account disabled after initial verify", sql: "UPDATE users SET status='DISABLED' WHERE id=?", want: domain.ErrAccountDisabled},
		{name: "session revoked after initial verify", sql: "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", want: domain.ErrSessionRevoked},
		{name: "device revoked after initial verify", sql: "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", want: domain.ErrDeviceRevoked},
		{name: "session expired after initial verify", sql: "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=?", want: domain.ErrSessionExpired},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			original := createFixture(t)
			changing := service.NewRefreshSessionService(func(ctx context.Context, raw string) (*domain.VerifiedRefreshSession, error) {
				verified, err := credentials.Verify(ctx, raw)
				if err != nil {
					return nil, err
				}
				id := original.verified.Session.ID
				if testCase.want == domain.ErrAccountDisabled {
					id = original.verified.Session.UserID
					t.Cleanup(func() {
						if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
							t.Error(err)
						}
					})
				}
				if testCase.want == domain.ErrDeviceRevoked {
					id = original.verified.Session.DeviceID
				}
				if _, err := pool.ExecContext(ctx, testCase.sql, id); err != nil {
					return nil, err
				}
				return verified, nil
			}, withTransaction)
			result, err := changing.Refresh(ctx, original.raw)
			if result != nil || !errors.Is(err, testCase.want) {
				t.Fatal("初次校验后的状态变化未被拒绝", err)
			}
			var hash []byte
			var generation uint64
			if err := pool.QueryRowContext(ctx, "SELECT refresh_token_hash,token_generation FROM auth_sessions WHERE id=?", original.verified.Session.ID).Scan(&hash, &generation); err != nil {
				t.Fatal(err)
			}
			if [sha256.Size]byte(hash) != original.verified.TokenHash || generation != 1 {
				t.Fatal("复查失败仍改变了摘要或代次")
			}
		})
	}
	t.Run("two fully verified concurrent requests have one committed result", func(t *testing.T) {
		original := createFixture(t)
		bothVerified := make(chan struct{})
		var verifiedCount atomic.Int32
		concurrent := service.NewRefreshSessionService(func(ctx context.Context, raw string) (*domain.VerifiedRefreshSession, error) {
			verified, err := credentials.Verify(ctx, raw)
			if err != nil {
				return nil, err
			}
			if verifiedCount.Add(1) == 2 {
				close(bothVerified)
			}
			select {
			case <-bothVerified:
				return verified, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}, withTransaction)
		type outcome struct {
			result *domain.RefreshSessionResult
			err    error
		}
		results := make(chan outcome, 2)
		for range 2 {
			go func() {
				result, err := concurrent.Refresh(ctx, original.raw)
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
						t.Fatal("并发成功结果不正确")
					}
					if verified, err := credentials.Verify(ctx, received.result.RefreshToken); err != nil || verified == nil || verified.TokenGeneration != 2 {
						t.Fatal("胜者原文无法定位已提交结果", err)
					}
				} else {
					rejections++
					if received.result != nil || !errors.Is(received.err, domain.ErrInvalidRefreshToken) {
						t.Fatal("并发失败返回了凭证或错误异常", received.err)
					}
				}
			case <-time.After(6 * time.Second):
				t.Fatal("并发刷新未结束")
			}
		}
		if wins != 1 || rejections != 1 || verifiedCount.Load() != 2 {
			t.Fatal("同旧原文的两个初次校验未得到单次成功")
		}
	})
}
