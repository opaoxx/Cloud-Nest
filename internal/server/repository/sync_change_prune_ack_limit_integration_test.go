package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestReadSyncChangePruneAckLimitIntegration(t *testing.T) {
	// 1. 在独占库用正式表准备两个账号及不同的设备状态。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	installChangeEventFixtureTable(t, ctx, pool)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute(t, deviceDDL)
	execute(t, "UPDATE users SET change_seq=22 WHERE id=1")
	execute(t, "UPDATE users SET change_seq=? WHERE id=2", uint64(math.MaxUint64))
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(1,?,11,1,'FIXTURE_CHANGE','{}')"
	for sequence := 18; sequence <= 22; sequence++ {
		execute(t, insertEvent, sequence)
	}
	execute(t, `INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at) VALUES
		(20,1,'28ee758a-0727-42de-ae7d-3bb33155e150','较快设备','WEB',NULL),
		(21,1,'28ee758a-0727-42de-ae7d-3bb33155e151','待重建设备','WEB',NULL),
		(22,1,'28ee758a-0727-42de-ae7d-3bb33155e152','历史撤销设备','WEB',UTC_TIMESTAMP(6)),
		(23,1,'28ee758a-0727-42de-ae7d-3bb33155e153','未同步设备','WEB',NULL),
		(29,2,'28ee758a-0727-42de-ae7d-3bb33155e154','其他账号设备','WEB',NULL)`)
	execute(t, `INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at) VALUES
		(19,1,'ACTIVE',20,UNHEX(REPEAT('11',16)),UTC_TIMESTAMP(6)-INTERVAL 91 DAY),
		(20,1,'ACTIVE',21,UNHEX(REPEAT('12',16)),UTC_TIMESTAMP(6)),
		(21,1,'RESET_REQUIRED',0,UNHEX(REPEAT('13',16)),UTC_TIMESTAMP(6)),
		(22,1,'ACTIVE',0,UNHEX(REPEAT('14',16)),UTC_TIMESTAMP(6))`)

	assertLimit := func(t *testing.T, userID, want uint64, wantErr error) {
		t.Helper()
		var got uint64
		const allowDisabled = true
		err := store.withLockedUserTransaction(ctx, userID, allowDisabled, func(txContext context.Context, txStore *Store) error {
			var readErr error
			got, readErr = txStore.readSyncChangePruneAckLimit(txContext)
			return readErr
		})
		if got != want || !errors.Is(err, wantErr) {
			t.Fatalf("账号 %d 的确认上界为 %d，错误 %v，预期 %d 和 %v", userID, got, err, want, wantErr)
		}
	}

	// 2. 确认慢设备、重建及账号隔离的规则，并用第二连接核对账号锁。
	assertLimit(t, actor.UserID, 20, nil)
	assertLimit(t, 2, math.MaxUint64, nil)
	t.Run("disabled account and expired session", func(t *testing.T) {
		execute(t, "UPDATE users SET status='DISABLED' WHERE id=?", actor.UserID)
		execute(t, "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 HOUR WHERE id=?", actor.SessionID)
		t.Cleanup(func() {
			execute(t, "UPDATE users SET status='ACTIVE' WHERE id=?", actor.UserID)
			execute(t, "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=?", actor.SessionID)
		})
		assertLimit(t, actor.UserID, 20, nil)
	})
	if err := store.withLockedUserTransaction(ctx, actor.UserID, true, func(txContext context.Context, txStore *Store) error {
		limit, err := txStore.readSyncChangePruneAckLimit(txContext)
		if err != nil || limit != 20 {
			return fmt.Errorf("锁内查询结果不符：%d，%v", limit, err)
		}
		contender, err := pool.BeginTx(txContext, nil)
		if err != nil {
			return err
		}
		defer contender.Rollback()
		var accountID uint64
		err = contender.QueryRowContext(txContext, "SELECT id FROM users WHERE id=? FOR UPDATE NOWAIT", actor.UserID).Scan(&accountID)
		var lockError *mysql.MySQLError
		if !errors.As(err, &lockError) || lockError.Number != 3572 {
			return fmt.Errorf("锁内查询后未持有所属账号锁：%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("第二连接的 NOWAIT 确认读取后仍持有账号锁")
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		return txStore.ResetSyncDeviceBaseline(txContext, domain.SyncCursorGeneration{0x31, 0x32})
	}); err != nil {
		t.Fatal(err)
	}
	assertLimit(t, actor.UserID, 21, nil)
	execute(t, "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=20")
	assertLimit(t, actor.UserID, 22, nil)
	execute(t, "UPDATE user_devices SET revoked_at=NULL WHERE id=20")
	execute(t, "UPDATE sync_devices SET status='ACTIVE',ack_sequence=20 WHERE device_id=19")
	execute(t, `INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at)
		VALUES(29,2,'ACTIVE',?,UNHEX(REPEAT('21',16)),UTC_TIMESTAMP(6))`, uint64(math.MaxUint64))
	assertLimit(t, actor.UserID, 20, nil)
	assertLimit(t, 2, math.MaxUint64, nil)

	// 3. 拒绝事务外调用、取消以及损坏的数据，保持账号序号与日志。
	t.Run("invalid positions and generation", func(t *testing.T) {
		for _, position := range []uint64{16, 23} {
			execute(t, "UPDATE sync_devices SET ack_sequence=? WHERE device_id=20", position)
			assertLimit(t, actor.UserID, 0, domain.ErrInvalidSyncDeviceData)
		}
		execute(t, "UPDATE sync_devices SET ack_sequence=17,cursor_generation=UNHEX(REPEAT('00',16)) WHERE device_id=20")
		assertLimit(t, actor.UserID, 0, domain.ErrInvalidSyncDeviceData)
		execute(t, "UPDATE sync_devices SET cursor_generation=UNHEX(REPEAT('12',16)) WHERE device_id=20")
		assertLimit(t, actor.UserID, 17, nil)
	})
	if limit, err := store.readSyncChangePruneAckLimit(ctx); limit != 0 || err == nil {
		t.Fatalf("事务外查询未被拒绝：%d，%v", limit, err)
	}
	if err := store.withLockedUserTransaction(ctx, actor.UserID, true, func(txContext context.Context, txStore *Store) error {
		canceled, stop := context.WithCancel(txContext)
		stop()
		if limit, err := txStore.readSyncChangePruneAckLimit(canceled); limit != 0 || !errors.Is(err, context.Canceled) {
			return fmt.Errorf("查询取消未生效：%d，%v", limit, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var highWater, eventCount, devicePosition uint64
	err := pool.QueryRowContext(ctx, `SELECT change_seq, (SELECT COUNT(*) FROM change_events WHERE user_id=1),
		(SELECT ack_sequence FROM sync_devices WHERE device_id=20) FROM users WHERE id=1`).Scan(&highWater, &eventCount, &devicePosition)
	if err != nil || highWater != 22 || eventCount != 5 || devicePosition != 17 {
		t.Fatalf("只读查询后结果不符：%d，%d，%d，%v", highWater, eventCount, devicePosition, err)
	}

	// 4. 仅在隔离夹具解除约束，验证异常状态和无法匹配的设备归属不会被漏算。
	execute(t, "ALTER TABLE sync_devices DROP CHECK ck_sync_devices_status")
	execute(t, "UPDATE sync_devices SET status='UNKNOWN' WHERE device_id=20")
	assertLimit(t, actor.UserID, 0, domain.ErrInvalidSyncDeviceData)
	execute(t, "UPDATE sync_devices SET status='ACTIVE' WHERE device_id=20")
	execute(t, "ALTER TABLE sync_devices DROP FOREIGN KEY fk_sync_devices_owner_device")
	execute(t, "UPDATE sync_devices SET user_id=1 WHERE device_id=29")
	assertLimit(t, actor.UserID, 0, domain.ErrInvalidSyncDeviceData)
}
