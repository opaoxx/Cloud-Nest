package repository

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestListUserSessionsIntegration(t *testing.T) {
	// 1. 复用独占验证库，使用正式设备和会话表创建不同归属及状态的夹具。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	expiresAt := createdAt.Add(3 * time.Hour)
	seenAt, revokedAt := createdAt.Add(10*time.Minute), createdAt.Add(time.Hour)
	createSession := func(t *testing.T, userID uint64, installation, name, platform string) domain.UserSession {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
		var created *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installation, name, platform)
			if err != nil {
				return err
			}
			created, err = txStore.CreateLoginSession(txContext, device.ID, hash, expiresAt)
			return err
		})
		if err != nil || created == nil {
			t.Fatal("创建会话列表夹具失败", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET created_at=? WHERE id=?", createdAt, created.ID); err != nil {
			t.Fatal(err)
		}
		return domain.UserSession{
			ID: created.ID, UserID: userID, DeviceID: created.DeviceID,
			DeviceName: name, DevicePlatform: platform, CreatedAt: createdAt, ExpiresAt: expiresAt,
		}
	}
	installation := uuid.NewString()
	active := createSession(t, 1, installation, "学习设备 📁", "WEB")
	revoked := createSession(t, 1, installation, "学习设备 📁", "WEB")
	expired := createSession(t, 1, installation, "学习设备 📁", "WEB")
	deviceRevoked := createSession(t, 1, uuid.NewString(), "桌面设备", "WINDOWS")
	foreign := createSession(t, 3, uuid.NewString(), "其他账号设备", "ANDROID")
	if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET last_seen_at=?,revoked_at=? WHERE id=?", seenAt, revokedAt, revoked.ID); err != nil {
		t.Fatal(err)
	}
	revoked.LastSeenAt, revoked.RevokedAt = &seenAt, &revokedAt
	expired.ExpiresAt = createdAt.Add(time.Minute)
	if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=? WHERE id=?", expired.ExpiresAt, expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=? WHERE id=?", revokedAt, deviceRevoked.DeviceID); err != nil {
		t.Fatal(err)
	}
	deviceRevoked.DeviceRevokedAt = &revokedAt
	checkPage := func(t *testing.T, userID, beforeID uint64, pageSize int, expected []domain.UserSession, hasMore bool) {
		t.Helper()
		page, err := store.ListUserSessions(ctx, userID, beforeID, pageSize)
		if err != nil || page == nil || page.Sessions == nil || !reflect.DeepEqual(page.Sessions, expected) || page.HasMore != hasMore {
			t.Fatal("真实会话列表的归属、分页、字段或状态错误", err)
		}
	}
	t.Run("all states and deterministic order", func(t *testing.T) {
		checkPage(t, 1, 0, 100, []domain.UserSession{deviceRevoked, expired, revoked, active}, false)
	})
	t.Run("owner scope and empty pages", func(t *testing.T) {
		checkPage(t, 3, 0, 100, []domain.UserSession{foreign}, false)
		checkPage(t, 99, 0, 100, []domain.UserSession{}, false)
		checkPage(t, 1, active.ID, 100, []domain.UserSession{}, false)
	})
	var newest domain.UserSession
	t.Run("pagination while newer session is inserted", func(t *testing.T) {
		checkPage(t, 1, 0, 2, []domain.UserSession{deviceRevoked, expired}, true)
		// 第二页继续使用第一页末项 ID，新插入的更大 ID 由后续首页读取。
		newest = createSession(t, 1, installation, "学习设备 📁", "WEB")
		checkPage(t, 1, expired.ID, 2, []domain.UserSession{revoked, active}, false)
		checkPage(t, 1, 0, 2, []domain.UserSession{newest, deviceRevoked}, true)
		// 其他用户的主键也只能作为数值边界，归属仍由 user_id 限定。
		checkPage(t, 1, foreign.ID, 2, []domain.UserSession{deviceRevoked, expired}, true)
	})
	// 2. 写事务持有会话及设备行时，普通列表读取已提交快照。
	t.Run("snapshot read while writer holds rows", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		baseline, err := store.ListUserSessions(ctx, 1, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE user_devices SET name='修改后的设备' WHERE id=?", newest.DeviceID); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=? WHERE id=?", revokedAt, newest.ID); err != nil {
			t.Fatal(err)
		}
		readContext, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		defer stop()
		beforeCommit, err := store.ListUserSessions(readContext, 1, 0, 100)
		if err != nil || !reflect.DeepEqual(beforeCommit, baseline) {
			t.Fatal("普通列表未在短期限内取得已提交快照", err)
		}
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		afterCommit, err := store.ListUserSessions(ctx, 1, 0, 100)
		if err != nil || afterCommit == nil || len(afterCommit.Sessions) != 5 {
			t.Fatal("提交后列表读取失败", err)
		}
		latest := afterCommit.Sessions[0]
		if latest.ID != newest.ID || latest.DeviceName != "修改后的设备" || latest.RevokedAt == nil || !latest.RevokedAt.Equal(revokedAt) {
			t.Fatal("提交后的设备与会话状态未反映到列表")
		}
	})
	// 3. 最后写入最大无符号标识，核对真实参数、扫描与游标比较。
	t.Run("full unsigned identifiers", func(t *testing.T) {
		identifier := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,status) VALUES(?,'fixture_max','fixture_hash','ACTIVE')", identifier); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,?,'最大标识设备','ANDROID')", identifier, identifier, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		_, hash := service.GenerateRefreshToken()
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,created_at,expires_at) VALUES(?,?,?,?,?,?)", identifier, identifier, identifier, hash[:], createdAt, expiresAt); err != nil {
			t.Fatal(err)
		}
		maximum := domain.UserSession{ID: identifier, UserID: identifier, DeviceID: identifier, DeviceName: "最大标识设备", DevicePlatform: "ANDROID", CreatedAt: createdAt, ExpiresAt: expiresAt}
		checkPage(t, identifier, 0, 1, []domain.UserSession{maximum}, false)
		checkPage(t, identifier, identifier, 1, []domain.UserSession{}, false)
	})
	// 4. 读取完成后共享池仍可用，连接故障返回错误及空结果。
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("列表查询关闭了共享连接池", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if page, err := store.ListUserSessions(ctx, 1, 0, 50); page != nil || err == nil {
		t.Fatal("连接故障被当作空列表")
	}
}
