package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestListOfflineSyncDeviceCandidatesIntegration(t *testing.T) {
	// 1. 在独占库安装同步设备表，用 MySQL 算出固定的微秒截止时间。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到第 3 版同步设备表定义")
	}
	execute(deviceDDL)
	reference := time.Date(2026, 11, 23, 12, 0, 0, 123456000, time.UTC)
	var cutoff time.Time
	if err := pool.QueryRowContext(ctx, "SELECT DATE_SUB(CAST(? AS DATETIME(6)), INTERVAL 90 DAY)", reference).Scan(&cutoff); err != nil ||
		!cutoff.Equal(reference.AddDate(0, 0, -90)) {
		t.Fatalf("MySQL 截止时间不符：%v，错误 %v", cutoff, err)
	}
	if candidates, err := store.ListOfflineSyncDeviceCandidates(ctx, cutoff, 0, 1); err != nil || candidates == nil || len(candidates) != 0 {
		t.Fatalf("空库应交付空页：%v，错误 %v", candidates, err)
	}

	// 2. 准备两个账号的设备。所有设备都有近期认证活动，只有同步活动参与筛选。
	execute(`INSERT INTO users(id,username,display_name,password_hash,storage_quota,change_seq)
		VALUES (1,'offline_owner','离线扫描用户','fixture_hash',100,7),
		(2,'offline_other','另一用户','fixture_hash',100,7)`)
	beforeCutoff := cutoff.Add(-time.Microsecond)
	older := cutoff.Add(-time.Hour)
	generationFor := func(deviceID uint64) domain.SyncCursorGeneration {
		return domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, byte(deviceID)}
	}
	for index, fixture := range []struct {
		deviceID, userID uint64
		status           domain.SyncDeviceStatus
		lastSeen         time.Time
		revoked          bool
	}{
		{11, 1, domain.SyncDeviceStatusActive, cutoff, false},
		{12, 1, domain.SyncDeviceStatusActive, cutoff.Add(time.Microsecond), false},
		{13, 2, domain.SyncDeviceStatusActive, beforeCutoff, false},
		{17, 1, domain.SyncDeviceStatusActive, older, true},
		{21, 2, domain.SyncDeviceStatusResetRequired, older, false},
		{25, 1, "", time.Time{}, false},
		{30, 1, domain.SyncDeviceStatusActive, older, false},
		{45, 2, domain.SyncDeviceStatusActive, beforeCutoff, false},
		{math.MaxUint64, 1, domain.SyncDeviceStatusActive, older, false},
	} {
		var revokedAt any
		if fixture.revoked {
			revokedAt = reference
		}
		execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform,last_seen_at,revoked_at)
			VALUES (?,?,?,'Fixture','WEB',?,?)`, fixture.deviceID, fixture.userID,
			fmt.Sprintf("28ee758a-0727-42de-ae7d-%012x", index+1), reference, revokedAt)
		if fixture.status == "" {
			continue
		}
		generation := generationFor(fixture.deviceID)
		var ack uint64
		if fixture.status == domain.SyncDeviceStatusActive {
			ack = 7
		}
		execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at)
			VALUES (?,?,?,?,?,?)`, fixture.deviceID, fixture.userID, fixture.status, ack, generation[:], fixture.lastSeen)
	}

	// 3. 比对分页前后的行，核对边界、设备归属、完整代次以及最大设备 ID。
	readDeviceRows := func() []string {
		t.Helper()
		var result []string
		err := store.database.WithContext(ctx).Raw(`SELECT JSON_ARRAY(device.id,device.user_id,device.last_seen_at,
			device.revoked_at,device.updated_at,states.status,states.ack_sequence,HEX(states.cursor_generation),
			states.last_seen_at,states.created_at,states.updated_at)
			FROM user_devices AS device
			LEFT JOIN sync_devices AS states ON states.user_id=device.user_id AND states.device_id=device.id
			ORDER BY device.id`).Scan(&result).Error
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	initialRows := readDeviceRows()
	if len(initialRows) != 9 {
		t.Fatalf("只读复核夹具不完整：%d", len(initialRows))
	}
	want := []domain.OfflineSyncDeviceCandidate{
		{UserID: 2, DeviceID: 13, Generation: generationFor(13), LastSeenAt: beforeCutoff},
		{UserID: 1, DeviceID: 30, Generation: generationFor(30), LastSeenAt: older},
		{UserID: 2, DeviceID: 45, Generation: generationFor(45), LastSeenAt: beforeCutoff},
		{UserID: 1, DeviceID: math.MaxUint64, Generation: generationFor(math.MaxUint64), LastSeenAt: older},
	}
	assertPage := func(afterDeviceID uint64, limit int, expected []domain.OfflineSyncDeviceCandidate) {
		t.Helper()
		candidates, err := store.ListOfflineSyncDeviceCandidates(ctx, cutoff.In(time.FixedZone("fixture", 8*3600)), afterDeviceID, limit)
		if err != nil || candidates == nil || !slices.Equal(candidates, expected) {
			t.Fatalf("设备 ID %d 之后的候选不符：%+v，错误 %v", afterDeviceID, candidates, err)
		}
	}
	assertPage(0, 2, want[:2])
	assertPage(30, 2, want[2:])
	assertPage(math.MaxUint64, 2, nil)
	assertPage(0, domain.MaxOfflineSyncDeviceCandidates, want)
	if !slices.Equal(readDeviceRows(), initialRows) {
		t.Fatal("只读扫描改变了设备认证或同步记录")
	}

	// 4. 校验调用边界。无效代次使整页失败，前面已读的候选不交付。
	for _, limit := range []int{0, domain.MaxOfflineSyncDeviceCandidates + 1} {
		if candidates, err := store.ListOfflineSyncDeviceCandidates(ctx, cutoff, 0, limit); candidates != nil || err == nil {
			t.Fatalf("接受了无效页大小 %d：%v，错误 %v", limit, candidates, err)
		}
	}
	for _, invalidCutoff := range []time.Time{{}, cutoff.Add(time.Nanosecond)} {
		if candidates, err := store.ListOfflineSyncDeviceCandidates(ctx, invalidCutoff, 0, 1); candidates != nil || err == nil {
			t.Fatalf("接受了无效截止时间 %v：%v，错误 %v", invalidCutoff, candidates, err)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if candidates, err := store.ListOfflineSyncDeviceCandidates(canceled, cutoff, 0, 1); candidates != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("未交付取消原因：%v，错误 %v", candidates, err)
	}
	if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
		if candidates, err := txStore.ListOfflineSyncDeviceCandidates(txContext, cutoff, 0, 1); candidates != nil || err == nil {
			t.Error("事务中接受了跨账号扫描", candidates, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	execute("UPDATE sync_devices SET cursor_generation=UNHEX(REPEAT('00',16)) WHERE device_id=45")
	if candidates, err := store.ListOfflineSyncDeviceCandidates(ctx, cutoff, 0, 3); candidates != nil || !errors.Is(err, domain.ErrInvalidSyncDeviceData) {
		t.Fatalf("无效代次应拒绝整页：%v，错误 %v", candidates, err)
	}
	t.Logf("离线设备扫描截止 %s；两页为 [13 30] 和 [45 %d]；撤销、待重建、边界行被排除，前后设备记录一致",
		cutoff.Format(time.RFC3339Nano), uint64(math.MaxUint64))
}
