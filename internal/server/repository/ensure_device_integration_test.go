package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

// loginSessionDatabase 安装正式会话表，复用独占设备测试库的生命周期。
func loginSessionDatabase(t *testing.T) *sql.DB {
	t.Helper()
	pool := loginDeviceDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	if start < 0 {
		t.Fatal("未找到会话表定义")
	}
	end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE stored_objects (")
	if end < 0 {
		t.Fatal("未找到会话表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestEnsureLoginDeviceIntegration(t *testing.T) {
	// 1. 在独占库中使用真实设备和会话表，旧会话只由测试建立虚构记录。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ensure := func(userID uint64, installationID, name, platform string) (*domain.LoginDevice, error) {
		var device *domain.LoginDevice
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			var err error
			device, err = txStore.EnsureLoginDevice(txContext, installationID, name, platform)
			return err
		})
		if err != nil {
			return nil, err
		}
		return device, nil
	}
	installationID := uuid.NewString()
	pool.SetMaxOpenConns(1)
	original, err := ensure(1, installationID, "用户设备名称", "WEB")
	if err != nil || original == nil || original.ID == 0 {
		t.Fatalf("首次准备没有创建设备: %v", err)
	}
	readDeviceTimes := func(t *testing.T) (sql.NullTime, time.Time) {
		t.Helper()
		var revokedAt sql.NullTime
		var updatedAt time.Time
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at,updated_at FROM user_devices WHERE user_id=1 AND id=?", original.ID).Scan(&revokedAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		return revokedAt, updatedAt
	}
	count := func(t *testing.T, userID uint64, installation string) int {
		t.Helper()
		var total int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_devices WHERE user_id=? AND installation_id=?", userID, installation).Scan(&total); err != nil {
			t.Fatal(err)
		}
		return total
	}
	t.Run("reuse same id and user edited metadata", func(t *testing.T) {
		_, before := readDeviceTimes(t)
		device, err := ensure(1, installationID, "请求中的新名称", "WINDOWS")
		_, after := readDeviceTimes(t)
		if err != nil || device == nil || *device != *original || !before.Equal(after) || count(t, 1, installationID) != 1 {
			t.Fatalf("已有设备复用或名称保留错误: %v", err)
		}
	})

	// 2. 恢复同一设备，核对旧会话仍持有原撤销时间及轮换代次。
	revokedAt := time.Date(2026, 11, 10, 3, 0, 0, 123456000, time.UTC)
	t.Run("restore device while old session stays revoked", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=?,updated_at=? WHERE id=?", revokedAt, revokedAt, original.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(user_id,device_id,refresh_token_hash,token_generation,expires_at,revoked_at) VALUES(1,?,?,2,?,?)", original.ID, make([]byte, 32), time.Now().UTC().Add(time.Hour), revokedAt); err != nil {
			t.Fatal(err)
		}
		device, err := ensure(1, installationID, "登录自动名称", "ANDROID")
		if err != nil || device == nil || *device != *original {
			t.Fatalf("恢复后设备 ID 或已有属性改变: %v", err)
		}
		revocation, updatedAt := readDeviceTimes(t)
		if revocation.Valid || !updatedAt.After(revokedAt) || updatedAt.Location() != time.UTC {
			t.Fatal("设备恢复或 UTC 修改时间错误")
		}
		var sessionRevokedAt sql.NullTime
		var generation uint64
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at,token_generation FROM auth_sessions WHERE user_id=1 AND device_id=?", original.ID).Scan(&sessionRevokedAt, &generation); err != nil {
			t.Fatal(err)
		}
		if !sessionRevokedAt.Valid || !sessionRevokedAt.Time.Equal(revokedAt) || generation != 2 {
			t.Fatal("恢复设备改变了旧会话的撤销状态或代次")
		}
	})
	t.Run("rollback both restoration and creation", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=?,updated_at=? WHERE id=?", revokedAt, revokedAt, original.ID); err != nil {
			t.Fatal(err)
		}
		newInstallation := uuid.NewString()
		failure := errors.New("fixture later login failure")
		err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.EnsureLoginDevice(txContext, installationID, "fixture", "WEB"); err != nil {
				return err
			}
			if _, err := txStore.EnsureLoginDevice(txContext, newInstallation, "fixture", "ANDROID"); err != nil {
				return err
			}
			return failure
		})
		revocation, updatedAt := readDeviceTimes(t)
		if !errors.Is(err, failure) || !revocation.Valid || !revocation.Time.Equal(revokedAt) || !updatedAt.Equal(revokedAt) || count(t, 1, newInstallation) != 0 {
			t.Fatal("事务错误未回滚设备恢复和新建")
		}
	})
	t.Run("same installation for another user", func(t *testing.T) {
		device, err := ensure(3, installationID, "另一账号的设备", "WINDOWS")
		if err != nil || device == nil || device.UserID != 3 || device.ID == original.ID || count(t, 3, installationID) != 1 {
			t.Fatalf("设备跨账号归属错误: %v", err)
		}
		revocation, _ := readDeviceTimes(t)
		if !revocation.Valid {
			t.Fatal("另一账号的登录恢复了当前账号设备")
		}
	})
	t.Run("disabled user rejected", func(t *testing.T) {
		newInstallation := uuid.NewString()
		device, err := ensure(2, newInstallation, "fixture", "WEB")
		if device != nil || !errors.Is(err, domain.ErrAccountDisabled) || count(t, 2, newInstallation) != 0 {
			t.Fatal("禁用账号仍完成设备准备")
		}
	})

	// 3. 用户行锁串行化同账号并发，所有请求最终复用同一设备 ID。
	t.Run("concurrent first login gets same device", func(t *testing.T) {
		pool.SetMaxOpenConns(4)
		newInstallation := uuid.NewString()
		type outcome struct {
			device *domain.LoginDevice
			err    error
		}
		results := make(chan outcome, 6)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for range 6 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				device, err := ensure(1, newInstallation, "并发设备", "ANDROID")
				results <- outcome{device: device, err: err}
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		var expectedID uint64
		for result := range results {
			if result.err != nil || result.device == nil || result.device.UserID != 1 || result.device.ID == 0 {
				t.Errorf("并发设备准备失败: %v", result.err)
				continue
			}
			if expectedID == 0 {
				expectedID = result.device.ID
			}
			if result.device.ID != expectedID {
				t.Error("并发请求取得不同设备 ID")
			}
		}
		if expectedID == 0 || count(t, 1, newInstallation) != 1 {
			t.Fatal("并发生成了重复设备")
		}
	})
}
