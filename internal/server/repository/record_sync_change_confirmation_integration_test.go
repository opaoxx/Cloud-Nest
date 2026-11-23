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

func TestRecordSyncChangeConfirmationIntegration(t *testing.T) {
	// 1. 在独占库准备正式表及其他账号、同账号其他设备的合成行。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	_, deviceDDL, exists := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !exists {
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
	otherGeneration := generation
	otherGeneration[15] ^= 1
	const insert = `INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at,created_at,updated_at)
		VALUES (?,?,?,?,?,'2020-01-01','2020-01-01','2020-01-01')`
	execute(insert, 20, 1, domain.SyncDeviceStatusActive, 17, generation[:])
	execute(insert, 29, 2, domain.SyncDeviceStatusActive, 17, generation[:])
	execute("UPDATE users SET change_seq=? WHERE id=?", uint64(math.MaxUint64), actor.UserID)

	type storedDevice struct {
		domain.SyncDeviceState
		lastSeen, created, updated time.Time
	}
	read := func(deviceID uint64) storedDevice {
		t.Helper()
		var state storedDevice
		var generationBytes []byte
		err := pool.QueryRowContext(ctx, `SELECT status,cursor_generation,ack_sequence,last_seen_at,created_at,updated_at
			FROM sync_devices WHERE device_id=?`, deviceID).
			Scan(&state.Status, &generationBytes, &state.AckSequence, &state.lastSeen, &state.created, &state.updated)
		if err != nil || len(generationBytes) != len(state.Generation) {
			t.Fatalf("读取合成设备记录失败：%v", err)
		}
		state.Generation = domain.SyncCursorGeneration(generationBytes)
		return state
	}
	runUpdate := func(candidate domain.SyncCursorGeneration, previous, target uint64) (bool, error) {
		var matched bool
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var updateError error
			matched, updateError = txStore.RecordSyncChangeConfirmation(txContext, candidate, previous, target)
			return updateError
		})
		return matched, err
	}
	assertUpdate := func(candidate domain.SyncCursorGeneration, previous, target uint64, wantMatched bool, wantError error) {
		t.Helper()
		matched, err := runUpdate(candidate, previous, target)
		if matched != wantMatched || !errors.Is(err, wantError) {
			t.Fatalf("确认 %d → %d 的命中状态 %t，错误 %v，期望 %t 和 %v", previous, target, matched, err, wantMatched, wantError)
		}
	}
	neighbor, foreign := read(20), read(29)
	assertUpdate(generation, 17, 17, false, nil)
	execute(insert, actor.DeviceID, actor.UserID, domain.SyncDeviceStatusResetRequired, 0, generation[:])
	pending := read(actor.DeviceID)
	assertUpdate(generation, 0, 0, false, nil)
	if read(actor.DeviceID) != pending {
		t.Fatal("待确认设备记录发生变化")
	}
	execute("UPDATE sync_devices SET status='ACTIVE', ack_sequence=17 WHERE device_id=?", actor.DeviceID)
	active := read(actor.DeviceID)

	// 2. 将独占连接的时钟固定下来，分别验证推进、同一微秒重试和等值刷新。
	stamp := time.Now().UTC().Truncate(time.Second)
	execute("SET time_zone = '+05:30'")
	defer execute("SET time_zone = '+00:00'")
	execute("SET timestamp = ?", stamp.Unix())
	defer execute("SET timestamp = DEFAULT")
	assertUpdate(generation, 17, 19, true, nil)
	advanced := read(actor.DeviceID)
	want := active
	want.AckSequence, want.lastSeen, want.updated = 19, stamp, stamp
	if advanced != want {
		t.Fatalf("推进应只改变位置与 UTC 活动时间：实际 %+v，期望 %+v", advanced, want)
	}
	assertUpdate(generation, 19, 19, true, nil)
	if read(actor.DeviceID) != advanced {
		t.Fatal("同一微秒的等值确认改变了记录")
	}
	assertUpdate(otherGeneration, 19, 19, false, nil)
	assertUpdate(generation, 18, 18, false, nil)
	assertUpdate(generation, 17, 23, false, nil)
	assertUpdate(generation, 19, 18, false, domain.ErrInvalidSyncDeviceData)
	assertUpdate(domain.SyncCursorGeneration{}, 19, 20, false, domain.ErrInvalidSyncDeviceData)
	if read(actor.DeviceID) != advanced {
		t.Fatal("无效代次、原位置或回退请求改变了记录")
	}
	later := stamp.Add(time.Second)
	execute("SET timestamp = ?", later.Unix())
	assertUpdate(generation, 19, 19, true, nil)
	want.lastSeen, want.updated = later, later
	if refreshed := read(actor.DeviceID); refreshed != want {
		t.Fatalf("等值确认应只更新时间：实际 %+v，期望 %+v", refreshed, want)
	}

	// 3. 检查回滚、完整无符号上界及其他设备与日志保持原值。
	rollback := errors.New("合成确认事务回滚")
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		matched, err := txStore.RecordSyncChangeConfirmation(txContext, generation, 19, 20)
		if err != nil || !matched {
			t.Fatalf("回滚夹具未执行更新：%t，错误 %v", matched, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) || read(actor.DeviceID) != want {
		t.Fatal("外层回滚后没有保持原值", err)
	}
	assertUpdate(generation, 19, math.MaxUint64, true, nil)
	want.AckSequence = math.MaxUint64
	if read(actor.DeviceID) != want || read(20) != neighbor || read(29) != foreign {
		t.Fatal("最大无符号确认位置不符或其他设备被改动")
	}
	var highWater, eventCount uint64
	if err := pool.QueryRowContext(ctx, `SELECT change_seq, (SELECT COUNT(*) FROM change_events WHERE user_id=1)
		FROM users WHERE id=1`).Scan(&highWater, &eventCount); err != nil || highWater != math.MaxUint64 || eventCount != 0 {
		t.Fatalf("确认后账号上界或事件数异常：上界 %d，事件数 %d，错误 %v", highWater, eventCount, err)
	}

	// 4. 普通仓储入口、未复核身份的事务和取消请求均无法更新。
	if matched, err := store.RecordSyncChangeConfirmation(ctx, generation, math.MaxUint64, math.MaxUint64); matched || err == nil {
		t.Fatal("普通仓储入口通过了事务检查")
	}
	err = store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
		_, err := txStore.RecordSyncChangeConfirmation(txContext, generation, math.MaxUint64, math.MaxUint64)
		return err
	})
	if err == nil {
		t.Fatal("未复核身份的事务通过了检查")
	}
	err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		canceled, stop := context.WithCancel(txContext)
		stop()
		_, err := txStore.RecordSyncChangeConfirmation(canceled, generation, math.MaxUint64, math.MaxUint64)
		return err
	})
	if !errors.Is(err, context.Canceled) || read(actor.DeviceID) != want {
		t.Fatal("取消请求错误不符或修改了设备记录", err)
	}
}
