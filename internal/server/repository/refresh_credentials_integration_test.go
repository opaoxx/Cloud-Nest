package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRefreshCredentialServiceIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	verifier := service.NewRefreshCredentialService(store.FindRefreshSession)
	type fixture struct {
		raw     string
		hash    [sha256.Size]byte
		session *domain.LoginSession
	}
	fixtures := make([]fixture, 3)
	// 1. 使用真实生成器建立三个独立设备会话，分别观察不同拒绝状态。
	if err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
		for index := range fixtures {
			fixtures[index].raw, fixtures[index].hash = service.GenerateRefreshToken()
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "刷新校验设备", "WEB")
			if err != nil {
				return err
			}
			fixtures[index].session, err = txStore.CreateLoginSession(txContext, device.ID, fixtures[index].hash, time.Now().UTC().Add(time.Hour))
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Run("active original token", func(t *testing.T) {
		verified, err := verifier.Verify(ctx, fixtures[0].raw)
		if err != nil || verified == nil || verified.Session != *fixtures[0].session || verified.TokenGeneration != 1 || verified.TokenHash != fixtures[0].hash {
			t.Fatal("真实生成、摘要定位与状态校验结果不一致")
		}
	})
	t.Run("committed digest replacement", func(t *testing.T) {
		newRaw, newHash := service.GenerateRefreshToken()
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET refresh_token_hash=?,token_generation=2 WHERE id=?", newHash[:], fixtures[0].session.ID); err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(ctx, fixtures[0].raw); result != nil || !errors.Is(err, domain.ErrInvalidRefreshToken) {
			t.Fatal("已替换原文仍被接受")
		}
		fixtures[0].raw, fixtures[0].hash = newRaw, newHash
		verified, err := verifier.Verify(ctx, newRaw)
		if err != nil || verified == nil || verified.TokenGeneration != 2 || verified.TokenHash != newHash {
			t.Fatal("新原文与代次校验错误")
		}
	})
	t.Run("disabled account", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(ctx, fixtures[0].raw); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("真实禁用账号未被拒绝")
		}
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("revoked session", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", fixtures[0].session.ID); err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(ctx, fixtures[0].raw); result != nil || !errors.Is(err, domain.ErrSessionRevoked) {
			t.Fatal("真实撤销会话未被拒绝")
		}
	})
	t.Run("expired session", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=? WHERE id=?", time.Now().UTC().Add(-time.Minute), fixtures[1].session.ID); err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(ctx, fixtures[1].raw); result != nil || !errors.Is(err, domain.ErrSessionExpired) {
			t.Fatal("真实到期会话未被拒绝")
		}
	})
	t.Run("revoked device", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", fixtures[2].session.DeviceID); err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(ctx, fixtures[2].raw); result != nil || !errors.Is(err, domain.ErrDeviceRevoked) {
			t.Fatal("真实撤销设备未被拒绝")
		}
	})
	t.Run("closed database stays an internal error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if result, err := verifier.Verify(ctx, fixtures[0].raw); result != nil || err == nil || errors.Is(err, domain.ErrInvalidRefreshToken) {
			t.Fatal("数据库故障被误判为凭证无效")
		}
	})
}
