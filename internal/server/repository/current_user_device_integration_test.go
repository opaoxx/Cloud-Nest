package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindCurrentUserDeviceIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	installSyncDeviceFixtureTable(t, ctx, pool)

	// 1. 准备同账号其他设备和另一账号设备，固定当前设备的时间字段。
	if _, err := pool.ExecContext(ctx, `INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
		(20,1,'d4f3e2e8-2d4f-4c29-899b-174ebe1d6c01','其他设备','WEB'),
		(21,2,'d4f3e2e8-2d4f-4c29-899b-174ebe1d6c02','外部账号设备','WEB')`); err != nil {
		t.Fatal(err)
	}
	seenAt := time.Date(2020, 2, 3, 4, 5, 6, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET last_seen_at=?, updated_at=? WHERE id=?", seenAt, seenAt, actor.DeviceID); err != nil {
		t.Fatal(err)
	}
	type deviceState struct {
		name                  string
		updatedAt             time.Time
		lastSeenAt, revokedAt sql.NullTime
	}
	readState := func() deviceState {
		t.Helper()
		var state deviceState
		err := pool.QueryRowContext(ctx, "SELECT name,updated_at,last_seen_at,revoked_at FROM user_devices WHERE id=?", actor.DeviceID).
			Scan(&state.name, &state.updatedAt, &state.lastSeenAt, &state.revokedAt)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	readCurrent := func() (*domain.UserDevice, error) {
		var device *domain.UserDevice
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var err error
			device, err = txStore.FindCurrentUserDevice(txContext)
			return err
		})
		return device, err
	}

	// 2. 单连接读取会话设备，读取前后保持原记录。
	before := readState()
	first, err := readCurrent()
	if err != nil || first == nil || first.ID != actor.DeviceID || first.UserID != actor.UserID ||
		first.Name != "回收设备" || first.Platform != "WEB" || first.CreatedAt.IsZero() ||
		first.LastSeenAt == nil || !first.LastSeenAt.Equal(seenAt) || first.RevokedAt != nil || readState() != before {
		t.Fatalf("设备快照或只读账面错误，得到 %+v 和 %v", first, err)
	}

	// 3. 通过正式改名提交后，重新读取数据库保存的名称，先前快照保持原值。
	const newName = "  学习电脑 🎮  "
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		_, err := txStore.RenameUserDevice(txContext, actor.DeviceID, newName)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	renamed := readState()
	second, err := readCurrent()
	if err != nil || second == nil || second.Name != newName || first.Name != before.name ||
		renamed.name != newName || readState() != renamed {
		t.Fatalf("快照未反映已提交的改名，得到 %+v 和 %v", second, err)
	}

	// 4. 验证事务边界、取消和设备撤销。
	if device, err := store.FindCurrentUserDevice(ctx); device != nil || err == nil {
		t.Fatal("事务外读取应被拒绝", err)
	}
	if err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
		_, err := txStore.FindCurrentUserDevice(txContext)
		return err
	}); err == nil {
		t.Fatal("登录事务缺少认证会话，应拒绝读取")
	}
	for _, deviceID := range []uint64{20, 21} {
		mismatchedActor := actor
		mismatchedActor.DeviceID = deviceID
		err := store.WithUserTransaction(ctx, mismatchedActor, func(context.Context, *Store) error {
			t.Error("其他设备身份不应进入读取回调")
			return nil
		})
		if !errors.Is(err, domain.ErrSessionNotFound) {
			t.Fatal("会话与设备不匹配未被拒绝", err)
		}
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		canceled, stop := context.WithCancel(txContext)
		stop()
		_, err := txStore.FindCurrentUserDevice(canceled)
		return err
	}); !errors.Is(err, context.Canceled) || readState() != renamed {
		t.Fatal("取消未保留原状态或错误原因", err)
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		if err := txStore.RevokeUserDevice(txContext, actor.DeviceID); err != nil {
			return err
		}
		if device, err := txStore.FindCurrentUserDevice(txContext); device != nil || !errors.Is(err, domain.ErrDeviceRevoked) {
			t.Fatalf("锁内读取未拒绝已撤销设备，得到 %+v 和 %v", device, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if device, err := readCurrent(); device != nil || !errors.Is(err, domain.ErrSessionRevoked) {
		t.Fatalf("撤销提交后旧会话仍取得快照，得到 %+v 和 %v", device, err)
	}
	t.Log("单连接读取当前设备，读到最新名称，拒绝不匹配设备、撤销及取消")
}
