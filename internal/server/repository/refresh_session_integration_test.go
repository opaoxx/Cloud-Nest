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

func TestFindRefreshSessionIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	oldHash := sha256.Sum256([]byte("fixture-current-refresh"))
	newHash := sha256.Sum256([]byte("fixture-next-refresh"))
	var device *domain.LoginDevice
	var session *domain.LoginSession
	if err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
		var err error
		device, err = txStore.EnsureLoginDevice(txContext, uuid.NewString(), "刷新测试设备", "WEB")
		if err != nil {
			return err
		}
		session, err = txStore.CreateLoginSession(txContext, device.ID, oldHash, time.Now().UTC().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := domain.RefreshSession{SessionAuthorization: domain.SessionAuthorization{UserID: 1, SessionID: session.ID, DeviceID: device.ID, UserStatus: "ACTIVE", SessionExpiresAt: session.ExpiresAt}, TokenGeneration: 1}
	check := func(t *testing.T, queryContext context.Context, hash [sha256.Size]byte, expected domain.RefreshSession) {
		t.Helper()
		actual, err := store.FindRefreshSession(queryContext, hash)
		if err != nil || !reflect.DeepEqual(actual, &expected) {
			t.Fatalf("真实刷新快照字段不一致: %v", err)
		}
	}
	t.Run("active snapshot and readonly timestamps", func(t *testing.T) {
		var before, after time.Time
		if err := pool.QueryRowContext(ctx, "SELECT updated_at FROM auth_sessions WHERE id=?", session.ID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, oldHash, want)
		check(t, ctx, oldHash, want)
		if err := pool.QueryRowContext(ctx, "SELECT updated_at FROM auth_sessions WHERE id=?", session.ID).Scan(&after); err != nil || !before.Equal(after) {
			t.Fatal("刷新定位查询改变了会话更新时间")
		}
		if result, err := store.FindRefreshSession(ctx, sha256.Sum256([]byte("fixture-missing"))); result != nil || !errors.Is(err, domain.ErrSessionNotFound) {
			t.Fatal("不存在摘要的查询结果错误")
		}
	})
	t.Run("disabled revoked and expired facts", func(t *testing.T) {
		revokedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		want.UserStatus = "DISABLED"
		check(t, ctx, oldHash, want)
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=? WHERE id=?", revokedAt, device.ID); err != nil {
			t.Fatal(err)
		}
		want.DeviceRevokedAt = &revokedAt
		check(t, ctx, oldHash, want)
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=?,expires_at=? WHERE id=?", revokedAt, revokedAt, session.ID); err != nil {
			t.Fatal(err)
		}
		want.SessionRevokedAt, want.SessionExpiresAt = &revokedAt, revokedAt
		check(t, ctx, oldHash, want)
	})
	t.Run("hash and generation follow committed state", func(t *testing.T) {
		// 按认证锁顺序构造轮换夹具，提交前应仍能按旧摘要读到上一份快照。
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		var lockedID uint64
		for _, statement := range []struct {
			query string
			id    uint64
		}{
			{query: "SELECT id FROM users WHERE id=? FOR UPDATE", id: 1},
			{query: "SELECT id FROM user_devices WHERE id=? FOR UPDATE", id: device.ID},
			{query: "SELECT id FROM auth_sessions WHERE id=? FOR UPDATE", id: session.ID},
		} {
			if err := transaction.QueryRowContext(ctx, statement.query, statement.id).Scan(&lockedID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := transaction.ExecContext(ctx, "UPDATE auth_sessions SET refresh_token_hash=?,token_generation=2 WHERE id=?", newHash[:], session.ID); err != nil {
			t.Fatal(err)
		}
		readContext, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		defer stop()
		check(t, readContext, oldHash, want)
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		if result, err := store.FindRefreshSession(ctx, oldHash); result != nil || !errors.Is(err, domain.ErrSessionNotFound) {
			t.Fatal("已替换摘要仍然命中")
		}
		want.TokenGeneration = 2
		check(t, ctx, newHash, want)
	})
	t.Run("another account has independent digest", func(t *testing.T) {
		otherHash := sha256.Sum256([]byte("fixture-other-account"))
		var otherSession *domain.LoginSession
		if err := store.WithLoginUserTransaction(ctx, 3, func(txContext context.Context, txStore *Store) error {
			otherDevice, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "另一账号设备", "ANDROID")
			if err != nil {
				return err
			}
			otherSession, err = txStore.CreateLoginSession(txContext, otherDevice.ID, otherHash, time.Now().UTC().Add(time.Hour))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		actual, err := store.FindRefreshSession(ctx, otherHash)
		if err != nil || actual == nil || actual.UserID != 3 || actual.SessionID != otherSession.ID || actual.DeviceID != otherSession.DeviceID {
			t.Fatal("摘要定位混用了其他账号资料")
		}
		check(t, ctx, newHash, want)
	})
	// 所有普通自增记录建立后，再验证完整 uint64 主键与代次。
	t.Run("maximum unsigned ids and generation", func(t *testing.T) {
		maximum := ^uint64(0)
		hash := sha256.Sum256([]byte("fixture-largest-record"))
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,status) VALUES(?,'fixture_max','fixture_hash','ACTIVE')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,?,'max','WEB')", maximum, maximum, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,token_generation,expires_at) VALUES(?,?,?,?,?,?)", maximum, maximum, maximum, hash[:], maximum, expiry); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, hash, domain.RefreshSession{SessionAuthorization: domain.SessionAuthorization{UserID: maximum, SessionID: maximum, DeviceID: maximum, UserStatus: "ACTIVE", SessionExpiresAt: expiry}, TokenGeneration: maximum})
	})
	t.Run("pool failure keeps error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if result, err := store.FindRefreshSession(ctx, newHash); result != nil || err == nil || errors.Is(err, domain.ErrSessionNotFound) {
			t.Fatal("数据库故障被当作未找到摘要")
		}
	})
}
