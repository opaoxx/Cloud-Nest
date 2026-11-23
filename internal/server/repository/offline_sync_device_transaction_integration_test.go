package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestWithOfflineSyncDeviceTransactionIntegration(t *testing.T) {
	// 1. 从独占库的真实扫描取得待复核设备。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到同步设备表定义")
	}
	execute(t, deviceDDL)
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil {
		t.Fatal(err)
	}
	cutoff := databaseTime.AddDate(0, 0, -90)
	oldActivity := cutoff.Add(-time.Microsecond)
	generation := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	otherGeneration := generation
	otherGeneration[15] ^= 1
	execute(t, "UPDATE users SET change_seq=7 WHERE id=?", actor.UserID)
	execute(t, `INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at)
		VALUES (?,?,'ACTIVE',7,?,?)`, actor.DeviceID, actor.UserID, generation[:], oldActivity)
	candidates, err := store.ListOfflineSyncDeviceCandidates(ctx, cutoff, 0, 1)
	if err != nil || len(candidates) != 1 || candidates[0].DeviceID != actor.DeviceID {
		t.Fatalf("读取初始候选失败：%+v，错误 %v", candidates, err)
	}
	candidate := candidates[0]
	readState := func(t *testing.T) string {
		t.Helper()
		var snapshot string
		err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(account.status,account.change_seq,
			device.last_seen_at,device.revoked_at,device.updated_at,states.status,states.ack_sequence,
			HEX(states.cursor_generation),states.last_seen_at,states.created_at,states.updated_at)
			FROM user_devices AS device JOIN users AS account ON account.id=device.user_id
			LEFT JOIN sync_devices AS states ON states.user_id=device.user_id AND states.device_id=device.id
			WHERE device.id=?`, actor.DeviceID).Scan(&snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	restoreSync := func(t *testing.T) {
		t.Helper()
		execute(t, "UPDATE sync_devices SET status='ACTIVE', ack_sequence=7, cursor_generation=?, last_seen_at=? WHERE device_id=?",
			generation[:], oldActivity, actor.DeviceID)
	}
	checkRejected := func(t *testing.T, queryContext context.Context, input domain.OfflineSyncDeviceCandidate, deadline time.Time, want error) {
		t.Helper()
		before := readState(t)
		err := store.WithOfflineSyncDeviceTransaction(queryContext, deadline, input, func(context.Context, *Store, domain.OfflineSyncDeviceCandidate) error {
			t.Error("未通过复核的候选进入了回调")
			return nil
		})
		if !errors.Is(err, want) || readState(t) != before {
			t.Fatalf("复核拒绝结果不符或改变了设备记录：%v，期望 %v", err, want)
		}
	}

	// 2. 在锁内回调用第二连接试锁账号、认证设备和同步行。
	t.Run("holds all locks through callback", func(t *testing.T) {
		before := readState(t)
		probes := []struct {
			statement string
			rowID     uint64
		}{
			{"SELECT id FROM users WHERE id=? FOR UPDATE NOWAIT", actor.UserID},
			{"SELECT id FROM user_devices WHERE id=? FOR UPDATE NOWAIT", actor.DeviceID},
			{"SELECT device_id FROM sync_devices WHERE device_id=? FOR UPDATE NOWAIT", actor.DeviceID},
		}
		err := store.WithOfflineSyncDeviceTransaction(ctx, cutoff, candidate, func(txContext context.Context, txStore *Store, current domain.OfflineSyncDeviceCandidate) error {
			if current != candidate || txStore.loginUserID != actor.UserID {
				return fmt.Errorf("回调未收到所属账号事务和锁内候选")
			}
			contender, err := pool.BeginTx(txContext, nil)
			if err != nil {
				return err
			}
			defer contender.Rollback()
			for _, probe := range probes {
				var lockedID uint64
				var lockError *mysql.MySQLError
				err := contender.QueryRowContext(txContext, probe.statement, probe.rowID).Scan(&lockedID)
				if !errors.As(err, &lockError) || lockError.Number != 3572 {
					return fmt.Errorf("回调期间未持有所需的行锁：%s，%v", probe.statement, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		contender, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer contender.Rollback()
		for _, probe := range probes {
			var lockedID uint64
			if err := contender.QueryRowContext(ctx, probe.statement, probe.rowID).Scan(&lockedID); err != nil || lockedID != probe.rowID {
				t.Fatalf("事务结束后仍无法锁定记录：%s，%v", probe.statement, err)
			}
		}
		if err := contender.Commit(); err != nil || readState(t) != before {
			t.Fatal("复核改变了设备记录或试锁事务未正常结束", err)
		}
		t.Log("账号、认证设备、同步行三次 NOWAIT 均确认回调期间持锁，事务结束后可重新锁定")
	})

	// 3. 通过现有业务事务提交确认或切换代次，再用原扫描候选复核。
	t.Run("confirmation committed after scan", func(t *testing.T) {
		t.Cleanup(func() { restoreSync(t) })
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			matched, err := txStore.RecordSyncChangeConfirmation(txContext, generation, 7, 7)
			if err != nil || !matched {
				return fmt.Errorf("等值增量确认未命中：%t，%v", matched, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		checkRejected(t, ctx, candidate, cutoff, domain.ErrOfflineSyncDeviceCandidateStale)
	})
	t.Run("snapshot generation changed after scan", func(t *testing.T) {
		t.Cleanup(func() { restoreSync(t) })
		if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return txStore.ResetSyncDeviceBaseline(txContext, otherGeneration)
		}); err != nil {
			t.Fatal(err)
		}
		checkRejected(t, ctx, candidate, cutoff, domain.ErrOfflineSyncDeviceCandidateStale)
		execute(t, "UPDATE sync_devices SET status='ACTIVE', ack_sequence=7, last_seen_at=? WHERE device_id=?", oldActivity, actor.DeviceID)
		checkRejected(t, ctx, candidate, cutoff, domain.ErrOfflineSyncDeviceCandidateStale)
	})
	t.Run("sync activity changed but still before cutoff", func(t *testing.T) {
		t.Cleanup(func() { restoreSync(t) })
		execute(t, "UPDATE sync_devices SET last_seen_at=? WHERE device_id=?", oldActivity.Add(-time.Microsecond), actor.DeviceID)
		checkRejected(t, ctx, candidate, cutoff, domain.ErrOfflineSyncDeviceCandidateStale)
	})
	t.Run("authentication activity and disabled account use maintenance rules", func(t *testing.T) {
		execute(t, "UPDATE user_devices SET last_seen_at=? WHERE id=?", databaseTime, actor.DeviceID)
		execute(t, "UPDATE users SET status='DISABLED' WHERE id=?", actor.UserID)
		t.Cleanup(func() {
			execute(t, "UPDATE users SET status='ACTIVE' WHERE id=?", actor.UserID)
		})
		before := readState(t)
		called := false
		err := store.WithOfflineSyncDeviceTransaction(ctx, cutoff, candidate, func(_ context.Context, _ *Store, current domain.OfflineSyncDeviceCandidate) error {
			called = current == candidate
			return nil
		})
		if err != nil || !called || readState(t) != before {
			t.Fatal("认证活动或禁用状态影响了后台复核", err)
		}
	})

	// 4. 区分普通跳过、无效数据与取消，确认无拒绝场景进入回调。
	t.Run("revoked after scan", func(t *testing.T) {
		execute(t, "UPDATE user_devices SET revoked_at=? WHERE id=?", databaseTime, actor.DeviceID)
		t.Cleanup(func() { execute(t, "UPDATE user_devices SET revoked_at=NULL WHERE id=?", actor.DeviceID) })
		checkRejected(t, ctx, candidate, cutoff, domain.ErrOfflineSyncDeviceCandidateStale)
	})
	checkRejected(t, ctx, candidate, oldActivity, domain.ErrOfflineSyncDeviceCandidateStale)
	wrongOwner := candidate
	wrongOwner.UserID = 2
	checkRejected(t, ctx, wrongOwner, cutoff, domain.ErrOfflineSyncDeviceCandidateStale)
	canceled, stop := context.WithCancel(ctx)
	stop()
	checkRejected(t, canceled, candidate, cutoff, context.Canceled)
	malformed := candidate
	malformed.Generation = domain.SyncCursorGeneration{}
	checkRejected(t, ctx, malformed, cutoff, domain.ErrInvalidSyncDeviceData)

	t.Run("corrupt stored state", func(t *testing.T) {
		execute(t, "UPDATE sync_devices SET status='RESET_REQUIRED' WHERE device_id=?", actor.DeviceID)
		t.Cleanup(func() { restoreSync(t) })
		checkRejected(t, ctx, candidate, cutoff, domain.ErrInvalidSyncDeviceData)
	})
	execute(t, "DELETE FROM sync_devices WHERE device_id=?", actor.DeviceID)
	checkRejected(t, ctx, candidate, cutoff, domain.ErrOfflineSyncDeviceCandidateStale)
	t.Log("确认、代次、同步时间、撤销、缺行和截止边界均按契约复核，拒绝候选未进入回调")
}
