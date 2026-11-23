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

func TestResetRestoredSyncDeviceIntegration(t *testing.T) {
	// 1. 准备活动、待重建及禁用账号下已撤销的恢复候选和需保留的业务状态。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute(deviceDDL)
	stamp := time.Date(2026, time.August, 25, 12, 0, 0, 123456000, time.UTC)
	newTime := time.Date(2026, time.November, 23, 12, 0, 0, 15625000, time.UTC)
	oldGeneration := domain.SyncCursorGeneration{0xa1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	execute("UPDATE users SET change_seq=5 WHERE id=1")
	execute("UPDATE users SET change_seq=7,status='DISABLED' WHERE id=2")
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at) VALUES
		(20,1,'28ee758a-0727-42de-ae7d-3bb33155e150','待重建','WEB',NULL),
		(29,2,'28ee758a-0727-42de-ae7d-3bb33155e159','已撤销','WEB',?)`, stamp)
	for _, fixture := range []struct {
		deviceID, userID uint64
		status           domain.SyncDeviceStatus
		ack              uint64
	}{
		{19, 1, domain.SyncDeviceStatusActive, 5},
		{20, 1, domain.SyncDeviceStatusResetRequired, 0},
		{29, 2, domain.SyncDeviceStatusActive, 7},
	} {
		execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?)`, fixture.deviceID, fixture.userID, fixture.status, fixture.ack, oldGeneration[:], stamp, stamp, stamp)
	}
	execute("INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload,created_at) "+
		"VALUES(1,5,12,1,'DIRECTORY_CREATED','{}',?)", stamp)
	candidates, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, 3)
	if err != nil || len(candidates) != 3 {
		t.Fatalf("未读取三条恢复候选：%+v，%v", candidates, err)
	}

	type storedDevice struct {
		domain.SyncDeviceState
		lastSeen, created, updated time.Time
	}
	readDevice := func(queryContext context.Context, readStore *Store, deviceID uint64) storedDevice {
		t.Helper()
		var data storedDevice
		var generation []byte
		err := readStore.database.WithContext(queryContext).Raw(`SELECT status,ack_sequence,cursor_generation,last_seen_at,created_at,updated_at
			FROM sync_devices WHERE device_id=?`, deviceID).Row().
			Scan(&data.Status, &data.AckSequence, &generation, &data.lastSeen, &data.created, &data.updated)
		if err != nil || len(generation) != len(data.Generation) {
			t.Fatalf("读取设备 %d 失败：%v", deviceID, err)
		}
		data.Generation = domain.SyncCursorGeneration(generation)
		return data
	}
	readProtected := func() string {
		t.Helper()
		var snapshot string
		err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(
			(SELECT JSON_ARRAY(change_seq,used_storage,reserved_storage,status) FROM users WHERE id=1),
			(SELECT JSON_ARRAY(change_seq,used_storage,reserved_storage,status) FROM users WHERE id=2),
			(SELECT JSON_ARRAY(revoked_at,updated_at) FROM user_devices WHERE id=29),
			(SELECT JSON_ARRAY(revoked_at,updated_at) FROM auth_sessions WHERE id=?),
			(SELECT JSON_ARRAY(revision,status) FROM file_entries WHERE id=12),
			(SELECT COUNT(*) FROM change_events),
			(SELECT JSON_ARRAY(entry_id,event_type,payload,created_at) FROM change_events WHERE user_id=1)
		)`, actor.SessionID).Scan(&snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	active := candidates[0]
	original := readDevice(ctx, store, active.DeviceID)
	protected := readProtected()
	assertReset := func(after, before storedDevice) {
		t.Helper()
		if after.Status != domain.SyncDeviceStatusResetRequired || after.AckSequence != 0 ||
			after.Generation == before.Generation || after.Generation == (domain.SyncCursorGeneration{}) ||
			!after.lastSeen.Equal(before.lastSeen) || !after.created.Equal(before.created) || !after.updated.Equal(newTime) {
			t.Fatalf("提交或保持的同步字段不符：更新后 %+v，更新前 %+v", after, before)
		}
	}
	execute("SET time_zone = '+05:30'")
	defer execute("SET time_zone = '+00:00'")
	execute("SET timestamp = ?", float64(newTime.Unix())+0.015625)
	defer execute("SET timestamp = DEFAULT")

	// 2. 验证实际条件未命中，以及写入后主动返回错误时的回滚。
	err = store.WithRestoredSyncDeviceTransaction(ctx, active, func(txContext context.Context, txStore *Store, current domain.RestoredSyncDeviceCandidate) error {
		current.State.AckSequence--
		return txStore.resetRestoredSyncDevice(txContext, current)
	})
	if !errors.Is(err, domain.ErrRestoredSyncDeviceCandidateStale) || readDevice(ctx, store, active.DeviceID) != original {
		t.Fatalf("条件未命中时仍改变记录：%v", err)
	}
	rollback := errors.New("合成恢复重置回滚")
	err = store.WithRestoredSyncDeviceTransaction(ctx, active, func(txContext context.Context, txStore *Store, current domain.RestoredSyncDeviceCandidate) error {
		if err := txStore.resetRestoredSyncDevice(txContext, current); err != nil {
			return err
		}
		assertReset(readDevice(txContext, txStore, active.DeviceID), original)
		return rollback
	})
	if !errors.Is(err, rollback) || readDevice(ctx, store, active.DeviceID) != original {
		t.Fatalf("实际更新后没有回滚到原记录：%v", err)
	}

	// 3. 用正式入口依次提交，旧扫描结果再次提交时保留已有换代结果。
	for _, candidate := range candidates {
		before := readDevice(ctx, store, candidate.DeviceID)
		if before.SyncDeviceState != candidate.State {
			t.Fatalf("之前的提交改变了另一设备 %d：%+v", candidate.DeviceID, before)
		}
		if err := store.ResetRestoredSyncDevice(ctx, candidate); err != nil {
			t.Fatal(err)
		}
		committed := readDevice(ctx, store, candidate.DeviceID)
		assertReset(committed, before)
		if err := store.ResetRestoredSyncDevice(ctx, candidate); !errors.Is(err, domain.ErrRestoredSyncDeviceCandidateStale) ||
			readDevice(ctx, store, candidate.DeviceID) != committed {
			t.Fatalf("旧候选重试改变了设备 %d 已提交的代次：%v", candidate.DeviceID, err)
		}
	}
	if readProtected() != protected {
		t.Fatal("换代改变了账号序号、事件、配额、目录或认证撤销状态")
	}
	t.Log("锁内条件未命中和主动回滚保持原值，三个恢复候选分别换代，旧候选重试不改结果")
}
