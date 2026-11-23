package repository

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestListRestoredSyncDeviceCandidatesIntegration(t *testing.T) {
	// 1. 在独占库准备各类恢复设备，沿用正式同步表。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute(t, deviceDDL)
	assertPage := func(t *testing.T, after uint64, limit int, want ...domain.RestoredSyncDeviceCandidate) {
		t.Helper()
		got, err := store.ListRestoredSyncDeviceCandidates(ctx, after, limit)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("游标 %d 的恢复设备页不符：%+v，预期 %+v，错误 %v", after, got, want, err)
		}
	}
	assertPage(t, 0, 2)
	const largeDeviceID = uint64(math.MaxUint64 - 1)
	stamp := time.Date(2026, time.August, 25, 12, 0, 0, 123456000, time.UTC)
	execute(t, "UPDATE users SET change_seq=2 WHERE id=?", actor.UserID)
	execute(t, "UPDATE users SET change_seq=?,status='DISABLED' WHERE id=2", uint64(math.MaxUint64))
	execute(t, `INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at) VALUES
		(20,1,'28ee758a-0727-42de-ae7d-3bb33155e150','待重建','WEB',NULL),
		(21,1,'28ee758a-0727-42de-ae7d-3bb33155e151','历史撤销','WEB',?),
		(22,1,'28ee758a-0727-42de-ae7d-3bb33155e152','未同步','WEB',NULL),
		(?,2,'28ee758a-0727-42de-ae7d-3bb33155e153','禁用账号设备','WEB',NULL)`, stamp, largeDeviceID)
	generation := domain.SyncCursorGeneration{0x91, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfc}
	candidates := []domain.RestoredSyncDeviceCandidate{
		{UserID: actor.UserID, DeviceID: actor.DeviceID, State: domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation, AckSequence: 2}},
		{UserID: actor.UserID, DeviceID: 20, State: domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: generation}},
		{UserID: actor.UserID, DeviceID: 21, State: domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation, AckSequence: 1}},
		{UserID: 2, DeviceID: largeDeviceID, State: domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation, AckSequence: math.MaxUint64}},
	}
	for _, candidate := range candidates {
		execute(t, `INSERT INTO sync_devices(device_id,user_id,status,cursor_generation,ack_sequence,last_seen_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?)`, candidate.DeviceID, candidate.UserID, candidate.State.Status,
			candidate.State.Generation[:], candidate.State.AckSequence, stamp, stamp, stamp)
	}

	// 2. 核对稳定分页、不按活跃或撤销状态过滤，并从独立读取确认扫描没有写入。
	assertPage(t, 0, 2, candidates[:2]...)
	assertPage(t, 20, 2, candidates[2:]...)
	assertPage(t, 22, 1, candidates[3])
	assertPage(t, largeDeviceID, 2)
	assertPage(t, math.MaxUint64, 2)
	assertPage(t, 0, domain.MaxRestoredSyncDeviceCandidates, candidates...)
	var count, untouched uint64
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*),COUNT(CASE WHEN last_seen_at=? AND updated_at=? THEN 1 END) FROM sync_devices",
		stamp, stamp).Scan(&count, &untouched); err != nil || count != 4 || untouched != count {
		t.Fatalf("扫描后的同步行或时间不符：%d，%d，%v", count, untouched, err)
	}

	// 3. 拒绝异常输入和损坏的同步行，整页不交付部分候选。
	for _, limit := range []int{0, domain.MaxRestoredSyncDeviceCandidates + 1} {
		if got, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, limit); got != nil || err == nil {
			t.Fatalf("无效上限仍返回结果：%+v，%v", got, err)
		}
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if got, err := store.ListRestoredSyncDeviceCandidates(cancelled, 0, 2); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后仍返回候选：%+v，%v", got, err)
	}
	if err := store.withLockedUserTransaction(ctx, actor.UserID, true, func(txContext context.Context, txStore *Store) error {
		if got, err := txStore.ListRestoredSyncDeviceCandidates(txContext, 0, 2); got != nil || err == nil {
			t.Errorf("账号事务内仍执行跨账号扫描：%+v，%v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertInvalid := func(t *testing.T) {
		t.Helper()
		if got, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, 4); got != nil || !errors.Is(err, domain.ErrInvalidSyncDeviceData) {
			t.Fatalf("异常同步记录被漏过或返回部分候选：%+v，%v", got, err)
		}
	}
	execute(t, "UPDATE sync_devices SET ack_sequence=1 WHERE device_id=20")
	assertInvalid(t)
	execute(t, "UPDATE sync_devices SET ack_sequence=0 WHERE device_id=20")
	execute(t, "UPDATE sync_devices SET ack_sequence=3 WHERE device_id=21")
	assertInvalid(t)
	execute(t, "UPDATE sync_devices SET ack_sequence=1 WHERE device_id=21")
	execute(t, "UPDATE sync_devices SET cursor_generation=UNHEX(REPEAT('00',16)) WHERE device_id=20")
	assertInvalid(t)
	execute(t, "UPDATE sync_devices SET cursor_generation=? WHERE device_id=20", generation[:])
	execute(t, "ALTER TABLE sync_devices DROP CHECK ck_sync_devices_status")
	execute(t, "UPDATE sync_devices SET status='UNKNOWN' WHERE device_id=20")
	assertInvalid(t)
	execute(t, "UPDATE sync_devices SET status='RESET_REQUIRED' WHERE device_id=20")

	// 隔离夹具模拟错误的外键导入，确认不匹配的行与零 ID 都会被发现。
	execute(t, "ALTER TABLE sync_devices DROP FOREIGN KEY fk_sync_devices_owner_device")
	execute(t, "UPDATE sync_devices SET user_id=2 WHERE device_id=20")
	assertInvalid(t)
	execute(t, "UPDATE sync_devices SET user_id=1 WHERE device_id=20")
	execute(t, `INSERT INTO sync_devices(device_id,user_id,status,cursor_generation,last_seen_at)
		VALUES(0,1,'RESET_REQUIRED',?,?)`, generation[:], stamp)
	assertInvalid(t)
}
