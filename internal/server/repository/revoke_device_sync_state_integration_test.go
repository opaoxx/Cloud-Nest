package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func installSyncDeviceFixtureTable(t *testing.T, ctx context.Context, pool *sql.DB) {
	t.Helper()
	_, ddl, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到同步设备表定义")
	}
	if _, err := pool.ExecContext(ctx, ddl); err != nil {
		t.Fatal("安装同步设备夹具表失败", err)
	}
}

func TestRevokeUserDeviceSyncStateIntegration(t *testing.T) {
	// 1. 准备同账号目标、历史撤销设备、未同步设备和外账号设备。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	installSyncDeviceFixtureTable(t, ctx, pool)
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at) VALUES
		(20,1,'28ee758a-0727-42de-ae7d-3bb33155e149','目标设备','WEB',NULL),
		(21,1,'28ee758a-0727-42de-ae7d-3bb33155e150','历史撤销设备','WEB',?),
		(22,1,'28ee758a-0727-42de-ae7d-3bb33155e151','未同步设备','WEB',NULL),
		(29,2,'28ee758a-0727-42de-ae7d-3bb33155e152','外账号设备','WEB',NULL)`, stamp)
	execute(`INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES
		(20,1,20,UNHEX(REPEAT('20',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),
		(21,1,21,UNHEX(REPEAT('21',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),
		(29,2,29,UNHEX(REPEAT('29',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`)
	execute("UPDATE users SET change_seq=7 WHERE id IN (1,2)")
	const original = "F100030400060708090A0B0C0D0E0FFE"
	execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at,created_at,updated_at)
		SELECT id,user_id,IF(id=21,'RESET_REQUIRED','ACTIVE'),IF(id=21,0,7),UNHEX(?),?,?,?
		FROM user_devices WHERE id IN (19,20,21,29)`, original, stamp, stamp, stamp)

	type state struct {
		deviceRevoked, sessionRevoked sql.NullTime
		status                        domain.SyncDeviceStatus
		ackSequence                   uint64
		generation                    string
		lastSeen, created, updated    time.Time
	}
	read := func(queryContext context.Context, readStore *Store, deviceID uint64) state {
		t.Helper()
		var saved state
		err := readStore.database.WithContext(queryContext).Raw(`SELECT device.revoked_at,session.revoked_at,
			sync.status,sync.ack_sequence,HEX(sync.cursor_generation),sync.last_seen_at,sync.created_at,sync.updated_at
			FROM user_devices AS device
			JOIN auth_sessions AS session ON session.user_id=device.user_id AND session.device_id=device.id
			JOIN sync_devices AS sync ON sync.user_id=device.user_id AND sync.device_id=device.id WHERE device.id=?`, deviceID).
			Row().Scan(&saved.deviceRevoked, &saved.sessionRevoked, &saved.status, &saved.ackSequence,
			&saved.generation, &saved.lastSeen, &saved.created, &saved.updated)
		if err != nil {
			t.Fatal("读取设备撤销及同步状态失败", err)
		}
		return saved
	}
	before := read(ctx, store, 20)
	caller, foreign := read(ctx, store, actor.DeviceID), read(ctx, store, 29)
	revoke := func(deviceID uint64) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return txStore.RevokeUserDevice(txContext, deviceID)
		})
	}
	assertInvalidated := func(earlier, current state) {
		t.Helper()
		if !current.deviceRevoked.Valid || !current.sessionRevoked.Valid || current.status != domain.SyncDeviceStatusResetRequired ||
			current.ackSequence != 0 || current.generation == earlier.generation || current.generation == strings.Repeat("0", 32) ||
			!current.lastSeen.Equal(earlier.lastSeen) || !current.created.Equal(earlier.created) || current.updated.Before(earlier.updated) {
			t.Fatalf("撤销或同步失效结果不符：%+v", current)
		}
	}

	// 2. 验证同步数据故障及成功写入后的外层回滚均恢复三类记录。
	execute("UPDATE sync_devices SET cursor_generation=UNHEX(REPEAT('00',16)) WHERE device_id=20")
	corrupt := read(ctx, store, 20)
	if err := revoke(20); !errors.Is(err, domain.ErrInvalidSyncDeviceData) || read(ctx, store, 20) != corrupt {
		t.Fatal("同步记录异常时认证写入没有一起回滚", err)
	}
	execute("UPDATE sync_devices SET cursor_generation=UNHEX(?) WHERE device_id=20", original)
	rollback := errors.New("合成撤销事务回滚")
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		if err := txStore.RevokeUserDevice(txContext, 20); err != nil {
			return err
		}
		assertInvalidated(before, read(txContext, txStore, 20))
		return rollback
	})
	if !errors.Is(err, rollback) || read(ctx, store, 20) != before {
		t.Fatal("外层回滚未恢复原设备、会话和同步行", err)
	}

	// 3. 提交并复验重复撤销、历史待重建记录和无同步记录的设备。
	if err := revoke(20); err != nil {
		t.Fatal(err)
	}
	committed := read(ctx, store, 20)
	assertInvalidated(before, committed)
	if err := revoke(20); err != nil {
		t.Fatal(err)
	}
	repeated := read(ctx, store, 20)
	assertInvalidated(committed, repeated)
	if repeated.deviceRevoked != committed.deviceRevoked || repeated.sessionRevoked != committed.sessionRevoked {
		t.Fatal("重复撤销改变了原撤销时间")
	}

	historical := read(ctx, store, 21)
	if err := revoke(21); err != nil {
		t.Fatal(err)
	}
	historicalAfter := read(ctx, store, 21)
	assertInvalidated(historical, historicalAfter)
	if historicalAfter.deviceRevoked != historical.deviceRevoked {
		t.Fatal("补齐历史同步失效时修改了设备撤销时间")
	}
	if err := revoke(22); err != nil {
		t.Fatal("无同步行的设备无法撤销", err)
	}
	var revoked sql.NullTime
	var syncRows int
	if err := pool.QueryRowContext(ctx, `SELECT device.revoked_at,
		(SELECT COUNT(*) FROM sync_devices WHERE user_id=device.user_id AND device_id=device.id)
		FROM user_devices AS device WHERE id=22`).Scan(&revoked, &syncRows); err != nil || !revoked.Valid || syncRows != 0 {
		t.Fatal("未同步设备的撤销状态不符", err)
	}
	if err := revoke(29); !errors.Is(err, domain.ErrDeviceNotFound) ||
		read(ctx, store, actor.DeviceID) != caller || read(ctx, store, 29) != foreign {
		t.Fatal("越权撤销未拒绝或其他设备受到影响", err)
	}
	t.Log("撤销与同步失效一起提交，故障和主动回滚恢复原值，历史与重复撤销保留撤销时间")
}
