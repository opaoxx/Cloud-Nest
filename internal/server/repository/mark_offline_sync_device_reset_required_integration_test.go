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

func TestMarkOfflineSyncDeviceResetRequiredIntegration(t *testing.T) {
	// 1. 安装正式同步表，准备候选、其他设备和一条已有变更。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到同步设备表定义")
	}
	execute(deviceDDL)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
		(20,1,'28ee758a-0727-42de-ae7d-3bb33155e149','Same account','WEB'),
		(29,2,'28ee758a-0727-42de-ae7d-3bb33155e14a','Other account','WEB')`)
	updateTime := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	cutoff := updateTime.AddDate(0, 0, -90)
	oldActivity := cutoff.Add(-time.Microsecond)
	createdAt := oldActivity.Add(-time.Hour)
	oldGeneration := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	execute("UPDATE users SET change_seq=7 WHERE id IN (1,2)")
	for _, device := range []struct{ userID, deviceID uint64 }{{actor.UserID, actor.DeviceID}, {1, 20}, {2, 29}} {
		execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at,created_at,updated_at)
			VALUES (?,?,'ACTIVE',7,?,?,?,?)`, device.deviceID, device.userID, oldGeneration[:], oldActivity, createdAt, oldActivity)
	}
	execute(`INSERT INTO change_events(user_id,sequence,entry_id,revision,event_type,payload,created_at)
		VALUES (1,7,12,1,'DIRECTORY_CREATED',JSON_OBJECT('entry_id','12','parent_id','11','name','正常目录','revision','1'),?)`, createdAt)

	candidates, err := store.ListOfflineSyncDeviceCandidates(ctx, cutoff, 0, 1)
	if err != nil || len(candidates) != 1 || candidates[0].DeviceID != actor.DeviceID {
		t.Fatalf("读取初始候选失败：%+v，错误 %v", candidates, err)
	}
	candidate := candidates[0]
	type deviceState struct {
		domain.SyncDeviceState
		lastSeen, created, updated time.Time
	}
	readDevice := func(queryContext context.Context, readStore *Store, deviceID uint64) deviceState {
		t.Helper()
		var state deviceState
		var generation []byte
		err := readStore.database.WithContext(queryContext).Raw(`SELECT status,ack_sequence,cursor_generation,last_seen_at,created_at,updated_at
			FROM sync_devices WHERE device_id=?`, deviceID).
			Row().Scan(&state.Status, &state.AckSequence, &generation, &state.lastSeen, &state.created, &state.updated)
		if err != nil || len(generation) != len(state.Generation) {
			t.Fatalf("读回同步设备 %d 失败：%v", deviceID, err)
		}
		state.Generation = domain.SyncCursorGeneration(generation)
		return state
	}
	readHistory := func() string {
		t.Helper()
		var snapshot string
		err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(account.change_seq,
			(SELECT COUNT(*) FROM change_events), events.sequence,events.entry_id,events.revision,events.event_type,events.payload,events.created_at,
			entry.status,entry.revision)
			FROM users AS account JOIN change_events AS events ON events.user_id=account.id
			JOIN file_entries AS entry ON entry.user_id=events.user_id AND entry.id=events.entry_id
			WHERE account.id=1 AND events.sequence=7`).Scan(&snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := readDevice(ctx, store, actor.DeviceID)
	neighbor, otherAccount := readDevice(ctx, store, 20), readDevice(ctx, store, 29)
	history := readHistory()
	assertMarked := func(state deviceState) {
		t.Helper()
		if state.Status != domain.SyncDeviceStatusResetRequired || state.AckSequence != 0 ||
			state.Generation == oldGeneration || state.Generation == (domain.SyncCursorGeneration{}) ||
			!state.lastSeen.Equal(before.lastSeen) || !state.created.Equal(before.created) || !state.updated.Equal(updateTime) {
			t.Fatalf("待重建字段或保持字段不符：%+v", state)
		}
	}

	// 2. 固定独占连接的时钟，并用非 UTC 会话验证更新时间仍为 UTC。
	execute("SET time_zone = '+05:30'")
	defer execute("SET time_zone = '+00:00'")
	execute("SET timestamp = ?", updateTime.Unix())
	defer execute("SET timestamp = DEFAULT")

	// 3. 验证候选过时和 SQL 条件不匹配均不会落库。
	execute("UPDATE sync_devices SET last_seen_at=? WHERE device_id=?", cutoff, actor.DeviceID)
	refreshed := readDevice(ctx, store, actor.DeviceID)
	if err := store.MarkOfflineSyncDeviceResetRequired(ctx, cutoff, candidate); !errors.Is(err, domain.ErrOfflineSyncDeviceCandidateStale) ||
		readDevice(ctx, store, actor.DeviceID) != refreshed {
		t.Fatal("已到截止点的候选被误更新", err)
	}
	execute("UPDATE sync_devices SET last_seen_at=? WHERE device_id=?", oldActivity, actor.DeviceID)
	err = store.WithOfflineSyncDeviceTransaction(ctx, cutoff, candidate, func(txContext context.Context, txStore *Store, current domain.OfflineSyncDeviceCandidate) error {
		current.LastSeenAt = current.LastSeenAt.Add(-time.Microsecond)
		return txStore.markOfflineSyncDeviceResetRequired(txContext, cutoff, current)
	})
	if !errors.Is(err, domain.ErrOfflineSyncDeviceCandidateStale) || readDevice(ctx, store, actor.DeviceID) != before {
		t.Fatal("条件更新未拒绝不匹配的活动时间", err)
	}

	// 4. 在实际更新后故意回滚，读库确认状态、代次和时间全部恢复。
	rollback := errors.New("合成离线重建回滚")
	err = store.WithOfflineSyncDeviceTransaction(ctx, cutoff, candidate, func(txContext context.Context, txStore *Store, current domain.OfflineSyncDeviceCandidate) error {
		if err := txStore.markOfflineSyncDeviceResetRequired(txContext, cutoff, current); err != nil {
			return err
		}
		assertMarked(readDevice(txContext, txStore, actor.DeviceID))
		return rollback
	})
	if !errors.Is(err, rollback) || readDevice(ctx, store, actor.DeviceID) != before {
		t.Fatal("实际更新后未回滚到原设备记录", err)
	}

	// 5. 走正式入口提交并重试，核对只更新目标设备同步行。
	if err := store.MarkOfflineSyncDeviceResetRequired(ctx, cutoff.In(time.FixedZone("fixture", 8*3600)), candidate); err != nil {
		t.Fatal(err)
	}
	committed := readDevice(ctx, store, actor.DeviceID)
	assertMarked(committed)
	if err := store.MarkOfflineSyncDeviceResetRequired(ctx, cutoff, candidate); !errors.Is(err, domain.ErrOfflineSyncDeviceCandidateStale) ||
		readDevice(ctx, store, actor.DeviceID) != committed {
		t.Fatal("重复候选改变了已提交的重建状态", err)
	}
	if readDevice(ctx, store, 20) != neighbor || readDevice(ctx, store, 29) != otherAccount || readHistory() != history {
		t.Fatal("标记待重建改变了其他设备、账号序号、变更事件或目录状态")
	}
	t.Log("提交后目标设备待重建、确认位置 0、代次更换；重复及未命中跳过，故意回滚恢复原记录，其他设备与事件原样")
}
