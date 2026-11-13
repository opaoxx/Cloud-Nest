package repository

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListUserDevicesIntegration(t *testing.T) {
	// 1. 复用独占库及正式设备表，创建不同归属和历史状态的设备。
	pool := loginDeviceDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	seenAt, revokedAt := createdAt.Add(10*time.Minute), createdAt.Add(time.Hour)
	createDevice := func(t *testing.T, userID uint64, installation, name, platform string) domain.UserDevice {
		t.Helper()
		var device *domain.LoginDevice
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			var err error
			device, err = txStore.CreateLoginDevice(txContext, installation, name, platform)
			return err
		})
		if err != nil || device == nil {
			t.Fatal("创建设备列表夹具失败", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET created_at=? WHERE user_id=? AND id=?", createdAt, userID, device.ID); err != nil {
			t.Fatal(err)
		}
		return domain.UserDevice{ID: device.ID, UserID: userID, Name: name, Platform: platform, CreatedAt: createdAt}
	}
	sharedInstallation := uuid.NewString()
	active := createDevice(t, 1, sharedInstallation, "学习浏览器 📁", "WEB")
	revoked := createDevice(t, 1, uuid.NewString(), "历史电脑", "WINDOWS")
	recent := createDevice(t, 1, uuid.NewString(), "最近手机", "ANDROID")
	foreign := createDevice(t, 3, sharedInstallation, "其他账号设备", "WEB")
	if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET last_seen_at=?,revoked_at=? WHERE id=?", seenAt, revokedAt, revoked.ID); err != nil {
		t.Fatal(err)
	}
	revoked.LastSeenAt, revoked.RevokedAt = &seenAt, &revokedAt
	if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET last_seen_at=? WHERE id=?", seenAt, recent.ID); err != nil {
		t.Fatal(err)
	}
	recent.LastSeenAt = &seenAt
	checkPage := func(t *testing.T, userID, beforeID uint64, pageSize int, expected []domain.UserDevice, hasMore bool) {
		t.Helper()
		page, err := store.ListUserDevices(ctx, userID, beforeID, pageSize)
		if err != nil || page == nil || page.Devices == nil || !reflect.DeepEqual(page.Devices, expected) || page.HasMore != hasMore {
			t.Fatal("真实设备列表的归属、字段或分页错误", err)
		}
	}
	t.Run("states and deterministic order", func(t *testing.T) {
		checkPage(t, 1, 0, 100, []domain.UserDevice{recent, revoked, active}, false)
	})
	t.Run("owner scope and empty pages", func(t *testing.T) {
		checkPage(t, 3, 0, 100, []domain.UserDevice{foreign}, false)
		checkPage(t, 99, 0, 100, []domain.UserDevice{}, false)
		checkPage(t, 1, active.ID, 100, []domain.UserDevice{}, false)
	})
	var newest domain.UserDevice
	t.Run("pagination while newer device is inserted", func(t *testing.T) {
		checkPage(t, 1, 0, 2, []domain.UserDevice{recent, revoked}, true)
		newest = createDevice(t, 1, uuid.NewString(), "新安装设备", "WINDOWS")
		checkPage(t, 1, revoked.ID, 2, []domain.UserDevice{active}, false)
		checkPage(t, 1, 0, 100, []domain.UserDevice{newest, recent, revoked, active}, false)
		checkPage(t, 1, foreign.ID, 2, []domain.UserDevice{recent, revoked}, true)
		checkPage(t, 1, ^uint64(0), 100, []domain.UserDevice{newest, recent, revoked, active}, false)
	})

	// 2. 写事务持有设备行时，列表读取已提交快照，提交后取得新值。
	t.Run("snapshot while writer holds device", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		baseline, err := store.ListUserDevices(ctx, 1, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE user_devices SET name=?,last_seen_at=?,revoked_at=? WHERE id=?", "修改后的设备", seenAt, revokedAt, newest.ID); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		beforeCommit, err := store.ListUserDevices(readContext, 1, 0, 100)
		if err != nil || !reflect.DeepEqual(beforeCommit, baseline) {
			t.Fatal("持有行锁时未及时读取已提交设备快照", err)
		}
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		newest.Name, newest.LastSeenAt, newest.RevokedAt = "修改后的设备", &seenAt, &revokedAt
		checkPage(t, 1, 0, 100, []domain.UserDevice{newest, recent, revoked, active}, false)
	})

	// 3. 最后写入最大无符号标识，检查真实参数、扫描及严格游标边界。
	t.Run("full unsigned identifiers", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,status) VALUES(?,'fixture_max','fixture_hash','ACTIVE')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform,created_at,last_seen_at) VALUES(?,?,?,'最大设备','ANDROID',?,?)", maximum, maximum, uuid.NewString(), createdAt, seenAt); err != nil {
			t.Fatal(err)
		}
		device := domain.UserDevice{ID: maximum, UserID: maximum, Name: "最大设备", Platform: "ANDROID", CreatedAt: createdAt, LastSeenAt: &seenAt}
		checkPage(t, maximum, 0, 1, []domain.UserDevice{device}, false)
		checkPage(t, maximum, maximum, 1, []domain.UserDevice{}, false)
		checkPage(t, 1, 0, 100, []domain.UserDevice{newest, recent, revoked, active}, false)
	})

	// 4. 检查共享池可继续使用，连接故障返回错误与空结果。
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("列表查询关闭了共享连接池", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if page, err := store.ListUserDevices(ctx, 1, 0, 50); page != nil || err == nil {
		t.Fatal("连接故障被当作空页面")
	}
}
