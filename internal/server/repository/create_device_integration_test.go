package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateLoginDeviceIntegration(t *testing.T) {
	// 1. 复用独占测试库及自动清理，仅替换该库的设备测试表为正式迁移中的原始定义。
	pool := loginTransactionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE user_devices (")
	if start < 0 {
		t.Fatal("迁移中未找到设备表")
	}
	end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE auth_sessions (")
	if end < 0 {
		t.Fatal("未找到设备表定义的结束边界")
	}
	for _, statement := range []string{"DROP TABLE user_devices", migrations.InitialSQL[start : start+end]} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	create := func(userID uint64, installationID, name, platform string) (*domain.LoginDevice, error) {
		var device *domain.LoginDevice
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			var err error
			device, err = txStore.CreateLoginDevice(txContext, installationID, name, platform)
			return err
		})
		if err != nil {
			return nil, err
		}
		return device, nil
	}
	count := func(userID uint64, installationID string) int {
		t.Helper()
		var total int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_devices WHERE user_id=? AND installation_id=?", userID, installationID).Scan(&total); err != nil {
			t.Fatal(err)
		}
		return total
	}
	installationID := uuid.NewString()
	var original *domain.LoginDevice

	// 2. 核对提交后的真实字段、自增主键和数据库默认值。
	t.Run("committed fields and defaults", func(t *testing.T) {
		// 只给一条连接，验证 INSERT 沿用用户事务，而没有去共享池另借连接。
		pool.SetMaxOpenConns(1)
		original, err = create(1, installationID, " 学习电脑💻 ", "WEB")
		if err != nil || original == nil || original.ID == 0 || original.UserID != 1 || original.RevokedAt != nil {
			t.Fatalf("创建设备或 ID 回填失败: %v", err)
		}
		var baseline domain.LoginDevice
		var osVersion, appVersion sql.NullString
		var lastSeenAt, revokedAt sql.NullTime
		var createdAt, updatedAt time.Time
		err = pool.QueryRowContext(ctx, "SELECT id,user_id,installation_id,name,platform,os_version,app_version,last_seen_at,revoked_at,created_at,updated_at FROM user_devices WHERE id=?", original.ID).
			Scan(&baseline.ID, &baseline.UserID, &baseline.InstallationID, &baseline.Name, &baseline.Platform, &osVersion, &appVersion, &lastSeenAt, &revokedAt, &createdAt, &updatedAt)
		if err != nil || !reflect.DeepEqual(&baseline, original) || baseline.Name != " 学习电脑💻 " || baseline.Platform != "WEB" {
			t.Fatalf("独立 SQL 读取与创建结果不一致: %v", err)
		}
		if osVersion.Valid || appVersion.Valid || lastSeenAt.Valid || revokedAt.Valid || createdAt.IsZero() || !createdAt.Equal(updatedAt) || createdAt.Location() != time.UTC {
			t.Fatal("NULL 或数据库 UTC 默认时间不正确")
		}
	})
	if original == nil {
		t.Fatal("缺少后续唯一约束测试所需的设备")
	}

	t.Run("revoked duplicate stays unchanged and rolls back earlier insert", func(t *testing.T) {
		revokedAt := time.Date(2026, 11, 10, 3, 0, 0, 123456000, time.UTC)
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=? WHERE id=?", revokedAt, original.ID); err != nil {
			t.Fatal(err)
		}
		original.RevokedAt = &revokedAt
		rolledBackInstallation := uuid.NewString()
		err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.CreateLoginDevice(txContext, rolledBackInstallation, "不能留下", "ANDROID"); err != nil {
				return err
			}
			device, err := txStore.CreateLoginDevice(txContext, installationID, "不能覆盖", "WINDOWS")
			if device != nil {
				t.Error("重复插入返回了设备对象")
			}
			return err
		})
		if !errors.Is(err, domain.ErrDeviceAlreadyExists) || strings.Contains(err.Error(), installationID) || count(1, rolledBackInstallation) != 0 || count(1, installationID) != 1 {
			t.Fatalf("唯一键冲突或事务回滚不正确: %v", err)
		}
		unchanged, err := store.FindLoginDeviceByInstallation(ctx, 1, installationID)
		if err != nil || !reflect.DeepEqual(unchanged, original) {
			t.Fatal("重复创建覆盖或恢复了已撤销设备")
		}
	})
	t.Run("same installation belongs to each user independently", func(t *testing.T) {
		device, err := create(3, installationID, strings.Repeat("学", 100), "WINDOWS")
		if err != nil || device == nil || device.UserID != 3 || device.ID == original.ID || count(3, installationID) != 1 || count(1, installationID) != 1 {
			t.Fatalf("跨账号安装标识或设备归属错误: %v", err)
		}
	})
	t.Run("callback error and panic roll back created device", func(t *testing.T) {
		failure := errors.New("fixture callback failure")
		for _, panicAfterInsert := range []bool{false, true} {
			newInstallation := uuid.NewString()
			func() {
				defer func() {
					payload := recover()
					if panicAfterInsert && payload != failure {
						t.Error("创建后的 panic 没有按原值传播")
					} else if !panicAfterInsert && payload != nil {
						t.Error("回调错误意外引起 panic")
					}
				}()
				err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
					if _, err := txStore.CreateLoginDevice(txContext, newInstallation, "回滚设备", "ANDROID"); err != nil {
						return err
					}
					if panicAfterInsert {
						panic(failure)
					}
					return failure
				})
				if !errors.Is(err, failure) {
					t.Errorf("回调错误原因丢失: %v", err)
				}
			}()
			if count(1, newInstallation) != 0 {
				t.Fatal("回调失败后创建的设备仍然存在")
			}
		}
	})
	t.Run("database constraint failure stays internal", func(t *testing.T) {
		newInstallation := uuid.NewString()
		device, err := create(1, newInstallation, "fixture", "UNKNOWN")
		if device != nil || err == nil || errors.Is(err, domain.ErrDeviceAlreadyExists) || count(1, newInstallation) != 0 {
			t.Fatal("CHECK 故障被当成重复设备或写入成功")
		}
	})
	t.Run("disabled user cannot create device", func(t *testing.T) {
		newInstallation := uuid.NewString()
		device, err := create(2, newInstallation, "fixture", "WEB")
		if device != nil || !errors.Is(err, domain.ErrAccountDisabled) || count(2, newInstallation) != 0 {
			t.Fatal("禁用账号创建设备未被阻止")
		}
	})

	// 3. 同账号并发只能创建一条，其他请求收到明确冲突；没有暗中复用或覆盖。
	t.Run("concurrent same user installation", func(t *testing.T) {
		pool.SetMaxOpenConns(4)
		newInstallation := uuid.NewString()
		start := make(chan struct{})
		results := make(chan error, 6)
		var workers sync.WaitGroup
		for range 6 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				device, err := create(1, newInstallation, "并发设备", "ANDROID")
				if err == nil && (device == nil || device.UserID != 1 || device.ID == 0) {
					err = fmt.Errorf("并发创建返回了错误设备")
				}
				results <- err
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		successes, duplicates := 0, 0
		for err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, domain.ErrDeviceAlreadyExists):
				duplicates++
			default:
				t.Errorf("并发创建内部故障: %v", err)
			}
		}
		if successes != 1 || duplicates != 5 || count(1, newInstallation) != 1 {
			t.Fatalf("并发唯一性不正确 success=%d duplicate=%d", successes, duplicates)
		}
	})
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("创建方法关闭了共享连接池")
	}
}
