package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindSessionAuthorizationIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	// 1. 在独占库中创建虚构设备与会话，生产查询使用普通 Store。
	var session *domain.LoginSession
	err = store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
		device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "fixture", "WEB")
		if err != nil {
			return err
		}
		session, err = txStore.CreateLoginSession(txContext, device.ID, sha256.Sum256([]byte("fixture-auth-token")), time.Now().UTC().Add(time.Hour))
		return err
	})
	if err != nil || session == nil {
		t.Fatalf("建立测试会话失败: %v", err)
	}
	expected := domain.SessionAuthorization{
		UserID: 1, SessionID: session.ID, DeviceID: session.DeviceID,
		UserStatus: "ACTIVE", SessionExpiresAt: session.ExpiresAt,
	}
	check := func(t *testing.T, queryContext context.Context, want domain.SessionAuthorization) {
		t.Helper()
		actual, err := store.FindSessionAuthorization(queryContext, want.UserID, want.SessionID)
		if err != nil || !reflect.DeepEqual(actual, &want) {
			t.Fatalf("实际数据库授权状态与预期不符: %v", err)
		}
	}
	t.Run("active fields and null revocations", func(t *testing.T) {
		check(t, ctx, expected)
	})
	t.Run("cross owner and missing session", func(t *testing.T) {
		for _, candidate := range []struct{ userID, sessionID uint64 }{
			{userID: 3, sessionID: session.ID},
			{userID: 1, sessionID: ^uint64(0)},
			{userID: ^uint64(0), sessionID: session.ID},
		} {
			actual, err := store.FindSessionAuthorization(ctx, candidate.userID, candidate.sessionID)
			if actual != nil || !errors.Is(err, domain.ErrSessionNotFound) {
				t.Fatal("跨账号或未找到结果错误")
			}
		}
	})

	// 2. 分别保存禁用、会话撤销、设备撤销和过期状态，确认查询保留事实。
	revokedAt := time.Date(2026, 11, 11, 3, 0, 0, 123456000, time.UTC)
	t.Run("committed status changes visible", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		expected.UserStatus = "DISABLED"
		check(t, ctx, expected)
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=? WHERE id=?", revokedAt, session.ID); err != nil {
			t.Fatal(err)
		}
		expected.SessionRevokedAt = &revokedAt
		check(t, ctx, expected)
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=? WHERE id=?", revokedAt, session.DeviceID); err != nil {
			t.Fatal(err)
		}
		expected.DeviceRevokedAt = &revokedAt
		check(t, ctx, expected)
		expiredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=? WHERE id=?", expiredAt, session.ID); err != nil {
			t.Fatal(err)
		}
		expected.SessionExpiresAt = expiredAt
		check(t, ctx, expected)
	})
	t.Run("read committed snapshot while writer holds rows", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
		if _, err := transaction.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.ExecContext(ctx, "UPDATE user_devices SET revoked_at=NULL WHERE id=?", session.DeviceID); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=NULL,expires_at=? WHERE id=?", future, session.ID); err != nil {
			t.Fatal(err)
		}
		// 写事务尚未提交时，普通查询读取上一份已提交状态，并能在短期限内完成。
		readContext, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		defer stop()
		check(t, readContext, expected)
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		expected.UserStatus = "ACTIVE"
		expected.SessionRevokedAt, expected.DeviceRevokedAt = nil, nil
		expected.SessionExpiresAt = future
		check(t, ctx, expected)
	})

	// 3. 最后写入完整无符号 ID 边界，确认三个表关联与扫描均支持 uint64。
	t.Run("full unsigned identifiers", func(t *testing.T) {
		identifier := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,status) VALUES(?, 'fixture_max', 'fixture_hash', 'ACTIVE')", identifier); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,?,'fixture','WEB')", identifier, identifier, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte("fixture-max-auth-token"))
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,?,?)", identifier, identifier, identifier, hash[:], session.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, domain.SessionAuthorization{UserID: identifier, SessionID: identifier, DeviceID: identifier, UserStatus: "ACTIVE", SessionExpiresAt: session.ExpiresAt})
	})
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("查询关闭了共享连接池")
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if actual, err := store.FindSessionAuthorization(ctx, 1, session.ID); actual != nil || err == nil || errors.Is(err, domain.ErrSessionNotFound) {
		t.Fatal("连接故障被误报为会话不存在")
	}
}
