package repository

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestActivateSyncDeviceBaselineIntegration(t *testing.T) {
	// 1. 安装正式同步设备表，准备具有相同代次的其他设备以检查写入范围。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(deviceDDL)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
		(20,1,'28ee758a-0727-42de-ae7d-3bb33155e149','Same account','WEB'),
		(29,2,'28ee758a-0727-42de-ae7d-3bb33155e14a','Other account','WEB')`)
	generation := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	nextGeneration := generation
	nextGeneration[15] ^= 1
	const insertPending = `INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at,created_at,updated_at)
		VALUES (?,?,?,'2020-01-01','2020-01-01','2020-01-01')`
	execute(insertPending, 20, 1, generation[:])
	execute(insertPending, 29, 2, generation[:])
	type storedState struct {
		domain.SyncDeviceState
		lastSeen, createdAt, updatedAt time.Time
	}
	read := func(deviceID uint64) storedState {
		t.Helper()
		var state storedState
		var generationBytes []byte
		err := pool.QueryRowContext(ctx, `SELECT status,cursor_generation,ack_sequence,last_seen_at,created_at,updated_at
			FROM sync_devices WHERE device_id=?`, deviceID).
			Scan(&state.Status, &generationBytes, &state.AckSequence, &state.lastSeen, &state.createdAt, &state.updatedAt)
		if err != nil || len(generationBytes) != len(state.Generation) {
			t.Fatalf("读取合成设备状态失败：%v", err)
		}
		state.Generation = domain.SyncCursorGeneration(generationBytes)
		return state
	}
	activate := func(candidate domain.SyncCursorGeneration, cutoff uint64) (bool, error) {
		var updated bool
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var updateError error
			updated, updateError = txStore.ActivateSyncDeviceBaseline(txContext, candidate, cutoff)
			return updateError
		})
		return updated, err
	}
	neighborBefore, otherBefore := read(20), read(29)

	// 2. 缺失和代次不匹配不更新。首次零序号可以激活，且只更新当前设备。
	if updated, err := activate(generation, 0); updated || err != nil {
		t.Fatalf("缺失记录的更新结果：%t，错误 %v", updated, err)
	}
	execute(insertPending, actor.DeviceID, actor.UserID, generation[:])
	pending := read(actor.DeviceID)
	if updated, err := activate(nextGeneration, 0); updated || err != nil || read(actor.DeviceID) != pending {
		t.Fatalf("末字节不同的代次修改了记录：%t，错误 %v", updated, err)
	}
	if updated, err := activate(generation, 0); !updated || err != nil {
		t.Fatalf("零序号激活失败：%t，错误 %v", updated, err)
	}
	active := read(actor.DeviceID)
	if active.SyncDeviceState != (domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation}) ||
		!active.createdAt.Equal(pending.createdAt) || !active.lastSeen.After(pending.lastSeen) || !active.updatedAt.Equal(active.lastSeen) {
		t.Fatalf("激活后的字段或时间不符：%+v", active)
	}
	if updated, err := activate(generation, 0); updated || err != nil || read(actor.DeviceID) != active {
		t.Fatalf("零序号重试改变了活动记录：%t，错误 %v", updated, err)
	}
	execute("UPDATE users SET change_seq=17 WHERE id=?", actor.UserID)
	execute("UPDATE sync_devices SET ack_sequence=17 WHERE device_id=?", actor.DeviceID)
	advanced := read(actor.DeviceID)
	if updated, err := activate(generation, 0); updated || err != nil || read(actor.DeviceID) != advanced {
		t.Fatalf("旧确认覆盖了已推进的位置：%t，错误 %v", updated, err)
	}

	// 3. 用新代次测试异常待确认位置、事务回滚和最大无符号序号。
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		return txStore.ResetSyncDeviceBaseline(txContext, nextGeneration)
	}); err != nil {
		t.Fatal(err)
	}
	pending = read(actor.DeviceID)
	if updated, err := activate(generation, 17); updated || err != nil || read(actor.DeviceID) != pending {
		t.Fatalf("旧代次修改了重建后的记录：%t，错误 %v", updated, err)
	}
	execute("UPDATE sync_devices SET ack_sequence=17 WHERE device_id=?", actor.DeviceID)
	invalidPending := read(actor.DeviceID)
	if updated, err := activate(nextGeneration, 17); updated || err != nil || read(actor.DeviceID) != invalidPending {
		t.Fatalf("待确认位置非零时执行了更新：%t，错误 %v", updated, err)
	}
	execute("UPDATE sync_devices SET ack_sequence=0 WHERE device_id=?", actor.DeviceID)
	execute("UPDATE users SET change_seq=? WHERE id=?", uint64(math.MaxUint64), actor.UserID)
	pending = read(actor.DeviceID)
	rollback := errors.New("夹具在激活后回滚")
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		updated, err := txStore.ActivateSyncDeviceBaseline(txContext, nextGeneration, math.MaxUint64)
		if err != nil || !updated {
			t.Fatalf("回滚夹具未执行更新：%t，错误 %v", updated, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) || read(actor.DeviceID) != pending {
		t.Fatalf("回滚后未恢复原状态：%v", err)
	}
	if updated, err := activate(nextGeneration, math.MaxUint64); !updated || err != nil {
		t.Fatalf("最大无符号序号激活失败：%t，错误 %v", updated, err)
	}
	expected := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: nextGeneration, AckSequence: math.MaxUint64}
	if current := read(actor.DeviceID); current.SyncDeviceState != expected || read(20) != neighborBefore || read(29) != otherBefore {
		t.Fatalf("提交位置不符或改动了其他设备：%+v", current)
	}

	// 4. 拒绝普通仓储、无认证会话的事务、取消调用和空代次。
	if updated, err := store.ActivateSyncDeviceBaseline(ctx, generation, 0); updated || err == nil {
		t.Fatal("普通 Store 通过了账号事务检查")
	}
	if err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
		_, err := txStore.ActivateSyncDeviceBaseline(txContext, generation, 0)
		return err
	}); err == nil {
		t.Fatal("无身份复核的事务通过了检查")
	}
	if updated, err := activate(domain.SyncCursorGeneration{}, 0); updated || !errors.Is(err, domain.ErrInvalidSyncDeviceData) {
		t.Fatalf("空代次未被拒绝：%t，错误 %v", updated, err)
	}
	err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		canceledContext, stop := context.WithCancel(txContext)
		stop()
		_, err := txStore.ActivateSyncDeviceBaseline(canceledContext, generation, 0)
		return err
	})
	if !errors.Is(err, context.Canceled) || read(actor.DeviceID).SyncDeviceState != expected {
		t.Fatalf("取消后的状态或错误不符：%v", err)
	}
}
