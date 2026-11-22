package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestResetSyncDeviceBaselineIntegration(t *testing.T) {
	// 1. 在独占库加载正式同步表，准备当前设备和另一账号的已同步设备。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	_, syncDevicesDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(syncDevicesDDL)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(29,2,'28ee758a-0727-42de-ae7d-3bb33155e149','Fixture','WEB')")
	otherGeneration := domain.SyncCursorGeneration{1, 2, 3, 0xff}
	execute("INSERT INTO sync_devices(device_id,user_id,ack_sequence,status,cursor_generation,last_seen_at) VALUES(29,2,8,'ACTIVE',?,UTC_TIMESTAMP(6))", otherGeneration[:])
	type syncDeviceState struct {
		owner, ack                     uint64
		status                         string
		generation                     domain.SyncCursorGeneration
		lastSeen, createdAt, updatedAt time.Time
	}
	readState := func(deviceID uint64) syncDeviceState {
		t.Helper()
		var state syncDeviceState
		var generation []byte
		err := pool.QueryRowContext(ctx, "SELECT user_id,ack_sequence,status,cursor_generation,last_seen_at,created_at,updated_at FROM sync_devices WHERE device_id=?", deviceID).
			Scan(&state.owner, &state.ack, &state.status, &generation, &state.lastSeen, &state.createdAt, &state.updatedAt)
		if err != nil || len(generation) != len(state.generation) {
			t.Fatalf("读取设备 %d 的同步状态失败：%v", deviceID, err)
		}
		state.generation = domain.SyncCursorGeneration(generation)
		return state
	}
	otherBefore := readState(29)
	first := domain.SyncCursorGeneration{0, 0xfe, 2, 0xff}
	next := first
	next[15] = 0x80
	rollback := errors.New("fixture rollback after reset")
	reset := func(identity domain.Actor, generation domain.SyncCursorGeneration, afterWrite func() error) error {
		return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
			if err := txStore.ResetSyncDeviceBaseline(txContext, generation); err != nil {
				return err
			}
			if afterWrite != nil {
				return afterWrite()
			}
			return nil
		})
	}

	// 2. 验证首次登记与重建的提交可见性，以及外层失败后恢复原状态。
	assertMissing := func() {
		t.Helper()
		if _, err := store.FindSyncDeviceGeneration(ctx, actor.UserID, actor.DeviceID); !errors.Is(err, domain.ErrSyncDeviceNotFound) {
			t.Fatalf("应当没有已提交的同步记录，错误 %v", err)
		}
	}
	if err := reset(actor, first, func() error { assertMissing(); return rollback }); !errors.Is(err, rollback) {
		t.Fatal("首次登记未经过预期回滚", err)
	}
	assertMissing()
	if err := reset(actor, first, func() error { assertMissing(); return nil }); err != nil {
		t.Fatal("首次登记未提交", err)
	}
	assertPending := func(state syncDeviceState, expected domain.SyncCursorGeneration) {
		t.Helper()
		if state.owner != actor.UserID || state.ack != 0 || state.status != "RESET_REQUIRED" || state.generation != expected ||
			state.lastSeen.IsZero() || state.createdAt.IsZero() || state.updatedAt.IsZero() ||
			state.lastSeen.Location() != time.UTC || state.lastSeen.Nanosecond()%1000 != 0 {
			t.Fatalf("待确认状态不符 %+v", state)
		}
	}
	assertPending(readState(actor.DeviceID), first)
	execute("UPDATE sync_devices SET ack_sequence=35,status='ACTIVE',last_seen_at='2020-01-01 00:00:00',updated_at='2020-01-01 00:00:00' WHERE device_id=?", actor.DeviceID)
	active := readState(actor.DeviceID)
	assertActive := func() {
		t.Helper()
		if state := readState(actor.DeviceID); state != active {
			t.Fatalf("其他连接读取的状态为 %+v，期望 %+v", state, active)
		}
	}
	if err := reset(actor, next, func() error { assertActive(); return rollback }); !errors.Is(err, rollback) {
		t.Fatal("重建未经过预期回滚", err)
	}
	assertActive()

	// 3. 拒绝未认证调用、跨账号设备及无效代次，原状态保持可读。
	if err := store.ResetSyncDeviceBaseline(ctx, next); err == nil {
		t.Fatal("普通 Store 执行了重置")
	}
	err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
		return txStore.ResetSyncDeviceBaseline(txContext, next)
	})
	if err == nil {
		t.Fatal("缺少认证会话的事务执行了重置")
	}
	wrongOwner := actor
	wrongOwner.DeviceID = 29
	if err := reset(wrongOwner, next, nil); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Fatal("跨账号设备拒绝不符", err)
	}
	for _, invalid := range []domain.SyncCursorGeneration{{}, first} {
		if err := reset(actor, invalid, nil); !errors.Is(err, domain.ErrInvalidSyncDeviceData) {
			t.Fatal("无效代次未被拒绝", err)
		}
	}
	assertActive()
	if err := reset(actor, next, nil); err != nil {
		t.Fatal("重建提交失败", err)
	}
	rebuilt := readState(actor.DeviceID)
	assertPending(rebuilt, next)
	if !rebuilt.createdAt.Equal(active.createdAt) || !rebuilt.updatedAt.After(active.updatedAt) || !rebuilt.lastSeen.After(active.lastSeen) {
		t.Fatalf("重建时间字段不符，之前 %+v，之后 %+v", active, rebuilt)
	}
	if actual := readState(29); actual != otherBefore {
		t.Fatalf("其他账号的设备状态发生变化 %+v", actual)
	}
}
