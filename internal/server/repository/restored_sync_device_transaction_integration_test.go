package repository

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestWithRestoredSyncDeviceTransactionIntegration(t *testing.T) {
	// 1. 在独占库取得真实扫描候选，包含已撤销且所属账号禁用的待重建记录。
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
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute(t, deviceDDL)
	stamp := time.Date(2026, time.August, 25, 12, 0, 0, 123456000, time.UTC)
	execute(t, "UPDATE users SET change_seq=2 WHERE id=?", actor.UserID)
	execute(t, "UPDATE users SET change_seq=3,status='DISABLED' WHERE id=2")
	execute(t, `INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at) VALUES
		(20,1,'28ee758a-0727-42de-ae7d-3bb33155e150','未同步','WEB',NULL),
		(29,2,'28ee758a-0727-42de-ae7d-3bb33155e159','已撤销','WEB',?)`, stamp)
	generation := domain.SyncCursorGeneration{0xa1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfc}
	execute(t, `INSERT INTO sync_devices(device_id,user_id,status,cursor_generation,ack_sequence,last_seen_at,created_at,updated_at)
		VALUES (19,1,'ACTIVE',?,2,?,?,?),(29,2,'RESET_REQUIRED',?,0,?,?,?)`, generation[:], stamp, stamp, stamp, generation[:], stamp, stamp, stamp)
	candidates, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, 2)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("未取得两条恢复候选：%+v，%v", candidates, err)
	}
	active := candidates[0]
	pending := candidates[1]

	// 2. 回调中用第二连接试锁三行，确认账号、认证设备和同步记录的锁都保持到事务结束。
	for _, candidate := range candidates {
		probes := []struct {
			statement string
			id        uint64
		}{
			{"SELECT id FROM users WHERE id=? FOR UPDATE NOWAIT", candidate.UserID},
			{"SELECT id FROM user_devices WHERE id=? FOR UPDATE NOWAIT", candidate.DeviceID},
			{"SELECT device_id FROM sync_devices WHERE device_id=? FOR UPDATE NOWAIT", candidate.DeviceID},
		}
		calls := 0
		err := store.WithRestoredSyncDeviceTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, current domain.RestoredSyncDeviceCandidate) error {
			calls++
			if txStore.loginUserID != candidate.UserID || txStore == store || current != candidate {
				return fmt.Errorf("回调未收到锁内复核结果和所属账号事务")
			}
			contender, err := pool.BeginTx(txContext, nil)
			if err != nil {
				return err
			}
			defer contender.Rollback()
			for _, probe := range probes {
				var lockedID uint64
				var lockError *mysql.MySQLError
				err := contender.QueryRowContext(txContext, probe.statement, probe.id).Scan(&lockedID)
				if !errors.As(err, &lockError) || lockError.Number != 3572 {
					return fmt.Errorf("复核时未持有所需的行锁：%s，%v", probe.statement, err)
				}
			}
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("设备 %d 未完成锁内复核：回调 %d 次，%v", candidate.DeviceID, calls, err)
		}
		for _, probe := range probes {
			var lockedID uint64
			if err := pool.QueryRowContext(ctx, probe.statement, probe.id).Scan(&lockedID); err != nil || lockedID != probe.id {
				t.Fatalf("复核事务结束后无法再次试锁：%s，%v", probe.statement, err)
			}
		}
	}
	t.Log("活动候选和已撤销待重建候选均在账号、认证设备及同步行锁内复核")
	after, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, 2)
	if err != nil || !slices.Equal(after, candidates) {
		t.Fatalf("只读复核改变了同步状态：%+v，%v", after, err)
	}
	var untouched int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM sync_devices WHERE last_seen_at=? AND updated_at=?", stamp, stamp).
		Scan(&untouched); err != nil || untouched != 2 {
		t.Fatalf("只读复核改变了活动或更新时间：%d，%v", untouched, err)
	}

	// 3. 拒绝变化的状态、原代次、确认位置和归属。拒绝时不执行回调。
	assertRejected := func(t *testing.T, queryContext context.Context, candidate domain.RestoredSyncDeviceCandidate, want error) {
		t.Helper()
		err := store.WithRestoredSyncDeviceTransaction(queryContext, candidate, func(context.Context, *Store, domain.RestoredSyncDeviceCandidate) error {
			t.Error("未通过复核的记录进入了回调")
			return nil
		})
		if !errors.Is(err, want) {
			t.Fatalf("拒绝原因不符：%v，期望 %v", err, want)
		}
	}
	for _, change := range []struct {
		name, update, restore string
		candidate             domain.RestoredSyncDeviceCandidate
		want                  error
	}{
		{"position changed", "UPDATE sync_devices SET ack_sequence=1 WHERE device_id=19", "UPDATE sync_devices SET ack_sequence=2 WHERE device_id=19", active, domain.ErrRestoredSyncDeviceCandidateStale},
		{"status changed", "UPDATE sync_devices SET status='ACTIVE' WHERE device_id=29", "UPDATE sync_devices SET status='RESET_REQUIRED' WHERE device_id=29", pending, domain.ErrRestoredSyncDeviceCandidateStale},
		{"upper bound reduced", "UPDATE users SET change_seq=1 WHERE id=1", "UPDATE users SET change_seq=2 WHERE id=1", active, domain.ErrInvalidSyncDeviceData},
		{"pending with nonzero position", "UPDATE sync_devices SET ack_sequence=1 WHERE device_id=29", "UPDATE sync_devices SET ack_sequence=0 WHERE device_id=29", pending, domain.ErrInvalidSyncDeviceData},
	} {
		t.Run(change.name, func(t *testing.T) {
			execute(t, change.update)
			t.Cleanup(func() { execute(t, change.restore) })
			assertRejected(t, ctx, change.candidate, change.want)
		})
	}
	t.Run("last byte of generation changed", func(t *testing.T) {
		changed := generation
		changed[15] ^= 1
		execute(t, "UPDATE sync_devices SET cursor_generation=? WHERE device_id=19", changed[:])
		t.Cleanup(func() { execute(t, "UPDATE sync_devices SET cursor_generation=? WHERE device_id=19", generation[:]) })
		assertRejected(t, ctx, active, domain.ErrRestoredSyncDeviceCandidateStale)
	})
	wrongOwner := active
	wrongOwner.UserID = 2
	assertRejected(t, ctx, wrongOwner, domain.ErrInvalidSyncDeviceData)
	missingSync := active
	missingSync.DeviceID = 20
	assertRejected(t, ctx, missingSync, domain.ErrSyncDeviceNotFound)
	invalid := active
	invalid.State.Generation = domain.SyncCursorGeneration{}
	assertRejected(t, ctx, invalid, domain.ErrInvalidSyncDeviceData)
	canceled, stop := context.WithCancel(ctx)
	stop()
	assertRejected(t, canceled, active, context.Canceled)
}
