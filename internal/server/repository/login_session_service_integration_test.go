package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// conflictingLoginSessionTransaction 在真实写入前替换摘要，触发数据库唯一键冲突。
type conflictingLoginSessionTransaction struct {
	service.LoginSessionTransaction
	hash [sha256.Size]byte
}

func (transaction *conflictingLoginSessionTransaction) CreateLoginSession(ctx context.Context, deviceID uint64, _ [sha256.Size]byte, expiresAt time.Time) (*domain.LoginSession, error) {
	return transaction.LoginSessionTransaction.CreateLoginSession(ctx, deviceID, transaction.hash, expiresAt)
}

func TestLoginSessionServiceIntegration(t *testing.T) {
	// 1. 复用独占库生命周期和正式设备、会话表，装配真实用户事务入口。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	jwtConfig := config.JWTConfig{
		SigningKey: bytes.Repeat([]byte{7}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 30 * 24 * time.Hour,
	}
	creator, err := service.NewLoginSessionService(jwtConfig, func(ctx context.Context, userID uint64, action func(context.Context, service.LoginSessionTransaction) error) error {
		return store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	installationID := uuid.NewString()
	before := time.Now().UTC()
	first, err := creator.Create(ctx, domain.VerifiedAccount{ID: 1}, installationID, "原始设备名称", "WEB")
	if err != nil || first == nil || first.Device.ID == 0 || first.Session.ID == 0 {
		t.Fatal("服务未成功提交设备与会话")
	}
	firstHash := sha256.Sum256([]byte(first.RefreshToken))
	t.Run("committed row and refresh hash", func(t *testing.T) {
		var userID, deviceID, generation uint64
		var hash []byte
		var expiresAt time.Time
		var revoked sql.NullTime
		err := pool.QueryRowContext(ctx, "SELECT user_id,device_id,refresh_token_hash,token_generation,expires_at,revoked_at FROM auth_sessions WHERE id=?", first.Session.ID).
			Scan(&userID, &deviceID, &hash, &generation, &expiresAt, &revoked)
		if err != nil || userID != 1 || deviceID != first.Device.ID || generation != 1 || revoked.Valid ||
			!bytes.Equal(hash, firstHash[:]) || !expiresAt.Equal(first.Session.ExpiresAt) || len(first.RefreshToken) != 43 {
			t.Fatal("提交后的会话字段与刷新凭证摘要不一致")
		}
		if expiresAt.Location() != time.UTC || expiresAt.Nanosecond()%1000 != 0 ||
			expiresAt.Before(before.Add(jwtConfig.RefreshTokenTTL).Truncate(time.Microsecond)) ||
			expiresAt.After(time.Now().UTC().Add(jwtConfig.RefreshTokenTTL)) {
			t.Fatal("会话期限未采用配置、UTC 与微秒精度")
		}
	})

	// 2. 同安装重新登录复用设备，恢复撤销设备时保留旧会话的撤销状态。
	second, err := creator.Create(ctx, domain.VerifiedAccount{ID: 1}, installationID, "请求中的新名称", "ANDROID")
	if err != nil || second == nil || second.Device.ID != first.Device.ID || second.Device.Name != first.Device.Name || second.Device.Platform != first.Device.Platform ||
		second.Session.ID == first.Session.ID || second.RefreshToken == first.RefreshToken {
		t.Fatal("重新登录未复用已有设备或未生成独立会话和凭证")
	}
	t.Run("restore device and preserve old revocations", func(t *testing.T) {
		revokedAt := time.Now().UTC().Truncate(time.Microsecond)
		if err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			if err := txStore.database.WithContext(txContext).Exec("UPDATE user_devices SET revoked_at=? WHERE id=?", revokedAt, first.Device.ID).Error; err != nil {
				return err
			}
			return txStore.database.WithContext(txContext).Exec("UPDATE auth_sessions SET revoked_at=? WHERE device_id=?", revokedAt, first.Device.ID).Error
		}); err != nil {
			t.Fatal("撤销测试夹具建立失败")
		}
		third, err := creator.Create(ctx, domain.VerifiedAccount{ID: 1}, installationID, "再次登录", "WEB")
		if err != nil || third == nil || third.Device.ID != first.Device.ID || third.Device.RevokedAt != nil {
			t.Fatal("服务未恢复原设备")
		}
		var oldRevoked, active int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM auth_sessions WHERE id IN (?,?) AND revoked_at=?", first.Session.ID, second.Session.ID, revokedAt).Scan(&oldRevoked); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM auth_sessions WHERE id=? AND revoked_at IS NULL", third.Session.ID).Scan(&active); err != nil || active != 1 || oldRevoked != 2 {
			t.Fatal("恢复设备改变了旧会话撤销状态或新会话状态异常")
		}
	})
	t.Run("same installation belongs to each user", func(t *testing.T) {
		other, err := creator.Create(ctx, domain.VerifiedAccount{ID: 3}, installationID, "其他账号设备", "WEB")
		if err != nil || other == nil || other.Device.ID == first.Device.ID || other.Device.UserID != 3 || other.Session.UserID != 3 {
			t.Fatal("相同安装标识跨账号复用了设备")
		}
	})

	// 3. 用真实摘要冲突验证整笔回滚，禁用及不存在账号在用户锁检查时拒绝。
	t.Run("session conflict rolls back new device", func(t *testing.T) {
		conflicting, err := service.NewLoginSessionService(jwtConfig, func(ctx context.Context, userID uint64, action func(context.Context, service.LoginSessionTransaction) error) error {
			return store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
				return action(txContext, &conflictingLoginSessionTransaction{LoginSessionTransaction: txStore, hash: firstHash})
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		installation := uuid.NewString()
		result, err := conflicting.Create(ctx, domain.VerifiedAccount{ID: 1}, installation, "待回滚设备", "WEB")
		if result != nil || !errors.Is(err, domain.ErrRefreshTokenConflict) {
			t.Fatal("真实摘要冲突未保留原因或返回了部分结果")
		}
		var devices int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_devices WHERE user_id=1 AND installation_id=?", installation).Scan(&devices); err != nil || devices != 0 {
			t.Fatal("会话写入失败后新设备没有回滚")
		}

		// 对已有撤销设备再触发冲突，恢复操作也应随同一事务回滚。
		revokedAt := time.Now().UTC().Truncate(time.Microsecond)
		if err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			return txStore.database.WithContext(txContext).Exec("UPDATE user_devices SET revoked_at=? WHERE id=?", revokedAt, first.Device.ID).Error
		}); err != nil {
			t.Fatal("撤销设备夹具建立失败")
		}
		result, err = conflicting.Create(ctx, domain.VerifiedAccount{ID: 1}, installationID, "待回滚恢复", "WEB")
		if result != nil || !errors.Is(err, domain.ErrRefreshTokenConflict) {
			t.Fatal("恢复设备后的会话冲突未正确返回")
		}
		var stillRevoked sql.NullTime
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at FROM user_devices WHERE id=?", first.Device.ID).Scan(&stillRevoked); err != nil || !stillRevoked.Valid || !stillRevoked.Time.Equal(revokedAt) {
			t.Fatal("失败事务未回滚设备恢复")
		}
	})
	for _, account := range []struct {
		name string
		id   uint64
		want error
	}{
		{name: "disabled", id: 2, want: domain.ErrAccountDisabled},
		{name: "missing", id: 99, want: domain.ErrAccountNotFound},
	} {
		t.Run(account.name, func(t *testing.T) {
			result, err := creator.Create(ctx, domain.VerifiedAccount{ID: account.id}, uuid.NewString(), "fixture", "WEB")
			if result != nil || !errors.Is(err, account.want) {
				t.Fatal("用户事务未复查账号状态或返回了部分结果")
			}
		})
	}

	// 4. 六个同安装并发登录串行取得用户锁，最终复用一个设备并各自创建会话。
	t.Run("concurrent same installation", func(t *testing.T) {
		installation := uuid.NewString()
		type outcome struct {
			result *domain.LoginSessionResult
			err    error
		}
		outcomes := make(chan outcome, 6)
		for range 6 {
			go func() {
				result, err := creator.Create(ctx, domain.VerifiedAccount{ID: 1}, installation, "并发设备", "WEB")
				outcomes <- outcome{result: result, err: err}
			}()
		}
		var deviceID uint64
		sessions := make(map[uint64]bool)
		tokens := make(map[string]bool)
		for range 6 {
			outcome := <-outcomes
			if outcome.err != nil || outcome.result == nil {
				t.Error("并发登录创建失败")
				continue
			}
			if deviceID == 0 {
				deviceID = outcome.result.Device.ID
			}
			if outcome.result.Device.ID != deviceID || sessions[outcome.result.Session.ID] || tokens[outcome.result.RefreshToken] {
				t.Error("并发登录未复用设备或复用了会话、凭证")
			}
			sessions[outcome.result.Session.ID], tokens[outcome.result.RefreshToken] = true, true
		}
		var devices, sessionCount int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_devices WHERE user_id=1 AND installation_id=?", installation).Scan(&devices); err != nil || devices != 1 {
			t.Fatal("并发登录没有持久化唯一设备")
		}
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM auth_sessions WHERE user_id=1 AND device_id=?", deviceID).Scan(&sessionCount); err != nil || sessionCount != 6 {
			t.Fatal("并发登录未持久化六个独立会话")
		}
	})
}
