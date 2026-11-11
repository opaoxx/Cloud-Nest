package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCreateLoginSessionIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().Add(30 * 24 * time.Hour).In(time.FixedZone("UTC+8", 8*60*60))
	tokenHash := sha256.Sum256([]byte("fixture-original-token"))
	installationID := uuid.NewString()
	var device *domain.LoginDevice
	var session *domain.LoginSession
	countHash := func(t *testing.T, hash [sha256.Size]byte) int {
		t.Helper()
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM auth_sessions WHERE refresh_token_hash=?", hash[:]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	countDevice := func(t *testing.T, installation string) int {
		t.Helper()
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_devices WHERE user_id=1 AND installation_id=?", installation).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	// 1. 一条连接内先准备设备再创建会话，提交后独立读取每个持久化字段。
	err = store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
		var err error
		device, err = txStore.EnsureLoginDevice(txContext, installationID, "fixture", "WEB")
		if err != nil {
			return err
		}
		session, err = txStore.CreateLoginSession(txContext, device.ID, tokenHash, expiresAt)
		return err
	})
	if err != nil || session == nil || session.ID == 0 || session.UserID != 1 || session.DeviceID != device.ID {
		t.Fatalf("设备与会话提交失败: %v", err)
	}
	t.Run("persisted fields and defaults", func(t *testing.T) {
		var baseline domain.LoginSession
		var storedHash []byte
		var generation uint64
		var lastSeenAt, revokedAt sql.NullTime
		var createdAt, updatedAt time.Time
		err := pool.QueryRowContext(ctx, "SELECT id,user_id,device_id,refresh_token_hash,token_generation,expires_at,last_seen_at,revoked_at,created_at,updated_at FROM auth_sessions WHERE id=?", session.ID).
			Scan(&baseline.ID, &baseline.UserID, &baseline.DeviceID, &storedHash, &generation, &baseline.ExpiresAt, &lastSeenAt, &revokedAt, &createdAt, &updatedAt)
		if err != nil || baseline != *session || !bytes.Equal(storedHash, tokenHash[:]) || len(storedHash) != 32 || baseline.ExpiresAt != expiresAt.UTC().Truncate(time.Microsecond) {
			t.Fatalf("会话字段或二进制摘要不一致: %v", err)
		}
		if generation != 1 || lastSeenAt.Valid || revokedAt.Valid || createdAt.IsZero() || !createdAt.Equal(updatedAt) || createdAt.Location() != time.UTC {
			t.Fatal("初始代次、NULL 或数据库 UTC 默认时间错误")
		}
	})
	t.Run("duplicate hash rolls back newly created device", func(t *testing.T) {
		newInstallation := uuid.NewString()
		err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			newDevice, err := txStore.EnsureLoginDevice(txContext, newInstallation, "fixture", "ANDROID")
			if err != nil {
				return err
			}
			_, err = txStore.CreateLoginSession(txContext, newDevice.ID, tokenHash, expiresAt)
			return err
		})
		if !errors.Is(err, domain.ErrRefreshTokenConflict) || strings.Contains(err.Error(), "Duplicate entry") || countDevice(t, newInstallation) != 0 || countHash(t, tokenHash) != 1 {
			t.Fatal("唯一键冲突没有保护原会话或回滚新设备")
		}
	})
	t.Run("later callback failure rolls back device and session", func(t *testing.T) {
		newInstallation := uuid.NewString()
		newHash := sha256.Sum256([]byte("fixture-rolled-back-token"))
		failure := errors.New("fixture later failure")
		err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			newDevice, err := txStore.EnsureLoginDevice(txContext, newInstallation, "fixture", "WINDOWS")
			if err != nil {
				return err
			}
			if _, err := txStore.CreateLoginSession(txContext, newDevice.ID, newHash, expiresAt); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) || countDevice(t, newInstallation) != 0 || countHash(t, newHash) != 0 {
			t.Fatal("后续失败没有一起回滚设备和会话")
		}
	})
	t.Run("missing and cross owner device", func(t *testing.T) {
		for _, candidate := range []struct{ userID, deviceID uint64 }{{userID: 3, deviceID: device.ID}, {userID: 1, deviceID: ^uint64(0)}} {
			err := store.WithLoginUserTransaction(ctx, candidate.userID, func(txContext context.Context, txStore *Store) error {
				_, err := txStore.CreateLoginSession(txContext, candidate.deviceID, tokenHash, expiresAt)
				return err
			})
			if !errors.Is(err, domain.ErrDeviceNotFound) || countHash(t, tokenHash) != 1 {
				t.Fatal("缺失或跨账号设备建立了会话")
			}
		}
	})

	// 2. 撤销设备及旧会话后，直接创建被拒绝，重新准备设备后可建立新会话。
	t.Run("restored device creates new session and preserves old revocation", func(t *testing.T) {
		revokedAt := time.Date(2026, 11, 11, 3, 0, 0, 123456000, time.UTC)
		for _, statement := range []struct {
			query string
			id    uint64
		}{
			{query: "UPDATE user_devices SET revoked_at=? WHERE id=?", id: device.ID},
			{query: "UPDATE auth_sessions SET revoked_at=? WHERE id=?", id: session.ID},
		} {
			if _, err := pool.ExecContext(ctx, statement.query, revokedAt, statement.id); err != nil {
				t.Fatal(err)
			}
		}
		newHash := sha256.Sum256([]byte("fixture-after-restoration"))
		err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			_, err := txStore.CreateLoginSession(txContext, device.ID, newHash, expiresAt)
			return err
		})
		if !errors.Is(err, domain.ErrDeviceRevoked) || countHash(t, newHash) != 0 {
			t.Fatal("撤销设备仍建立了会话")
		}
		var newSession *domain.LoginSession
		err = store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			restored, err := txStore.EnsureLoginDevice(txContext, installationID, "fixture", "WEB")
			if err != nil {
				return err
			}
			newSession, err = txStore.CreateLoginSession(txContext, restored.ID, newHash, expiresAt)
			return err
		})
		if err != nil || newSession == nil || newSession.ID == session.ID || newSession.DeviceID != device.ID || countHash(t, newHash) != 1 {
			t.Fatalf("恢复设备后新会话结果错误: %v", err)
		}
		var oldRevocation time.Time
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at FROM auth_sessions WHERE id=?", session.ID).Scan(&oldRevocation); err != nil || !oldRevocation.Equal(revokedAt) {
			t.Fatal("旧会话的撤销状态被恢复")
		}
	})
	t.Run("panic rolls back session", func(t *testing.T) {
		newHash := sha256.Sum256([]byte("fixture-panic-token"))
		func() {
			defer func() {
				if recover() != "fixture panic" {
					t.Error("panic 原值没有传播")
				}
			}()
			_ = store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.CreateLoginSession(txContext, device.ID, newHash, expiresAt); err != nil {
					return err
				}
				panic("fixture panic")
			})
		}()
		if countHash(t, newHash) != 0 {
			t.Fatal("panic 没有回滚会话")
		}
	})

	// 3. 并发争用同一摘要时，唯一索引只允许一条会话提交。
	t.Run("concurrent duplicate hash", func(t *testing.T) {
		pool.SetMaxOpenConns(4)
		newHash := sha256.Sum256([]byte("fixture-concurrent-token"))
		results := make(chan error, 6)
		var workers sync.WaitGroup
		for range cap(results) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				results <- store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
					_, err := txStore.CreateLoginSession(txContext, device.ID, newHash, expiresAt)
					return err
				})
			}()
		}
		workers.Wait()
		close(results)
		successes, conflicts := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, domain.ErrRefreshTokenConflict) {
				conflicts++
			} else {
				t.Errorf("并发写入出现内部错误: %v", err)
			}
		}
		if successes != 1 || conflicts != 5 || countHash(t, newHash) != 1 {
			t.Fatal("并发摘要唯一性不正确")
		}
	})
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("会话创建关闭了共享连接池")
	}
}
