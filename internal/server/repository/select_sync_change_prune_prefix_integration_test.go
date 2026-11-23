package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestSelectSyncChangePrunePrefixIntegration(t *testing.T) {
	// 1. 在独占库准备固定的微秒截止点、连续日志和两名合成账号。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
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
	cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 123456000, time.UTC)
	older := cutoff.Add(-time.Microsecond)
	newer := cutoff.Add(time.Microsecond)
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload,created_at) " +
		"VALUES(?,?,?,1,'FIXTURE_CHANGE','{}',?)"
	const setEventTime = "UPDATE change_events SET created_at=? WHERE user_id=1 AND `sequence`=?"
	const setSequence = "UPDATE change_events SET `sequence`=? WHERE user_id=1 AND `sequence`=?"
	const setHighWater = "UPDATE users SET change_seq=? WHERE id=?"
	const setPosition = "UPDATE sync_devices SET ack_sequence=? WHERE device_id=19"
	execute(t, setHighWater, 22, actor.UserID)
	for offset, createdAt := range []time.Time{older, cutoff, older, newer, older} {
		execute(t, insertEvent, actor.UserID, 18+offset, 11, createdAt)
	}
	execute(t, `INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at)
		VALUES(?,?,'ACTIVE',20,UNHEX(REPEAT('11',16)),?)`, actor.DeviceID, actor.UserID, cutoff)

	assertSelection := func(t *testing.T, userID uint64, deadline time.Time, limit int, want domain.SyncChangePruneRange, wantErr error) {
		t.Helper()
		var result domain.SyncChangePruneRange
		const allowDisabled = true
		err := store.withLockedUserTransaction(ctx, userID, allowDisabled, func(txContext context.Context, txStore *Store) error {
			var readErr error
			result, readErr = txStore.selectSyncChangePrunePrefix(txContext, deadline, limit)
			return readErr
		})
		if result != want || !errors.Is(err, wantErr) {
			t.Fatalf("账号 %d 筛选结果为 %+v，错误 %v，预期 %+v 和 %v", userID, result, err, want, wantErr)
		}
	}

	// 2. 验证时间边界、确认上界、批量上限及按所属账号隔离。
	assertSelection(t, actor.UserID, cutoff, 100, domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 18, Count: 1}, nil)
	execute(t, setEventTime, newer, 19)
	assertSelection(t, actor.UserID, cutoff, 100, domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 18, Count: 1}, nil)
	execute(t, setEventTime, newer, 18)
	assertSelection(t, actor.UserID, cutoff, 100, domain.SyncChangePruneRange{}, nil)
	execute(t, setEventTime, older, 18)
	execute(t, setEventTime, older, 19)
	assertSelection(t, actor.UserID, cutoff, 100, domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 20, Count: 3}, nil)
	assertSelection(t, actor.UserID, cutoff, 2, domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 19, Count: 2}, nil)
	execute(t, setPosition, 17)
	assertSelection(t, actor.UserID, cutoff, 100, domain.SyncChangePruneRange{}, nil)
	execute(t, setPosition, 20)

	execute(t, setHighWater, uint64(math.MaxUint64), 2)
	assertSelection(t, 2, cutoff, 100, domain.SyncChangePruneRange{}, nil)
	execute(t, insertEvent, 2, uint64(math.MaxUint64), 21, older)
	assertSelection(t, 2, cutoff, 100, domain.SyncChangePruneRange{FirstSequence: math.MaxUint64, LastSequence: math.MaxUint64, Count: 1}, nil)
	assertSelection(t, actor.UserID, cutoff, 100, domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 20, Count: 3}, nil)

	// 3. 构造可见断档或提前读尽，验证不会返回之前已选中的部分范围。
	execute(t, setHighWater, 23, actor.UserID)
	t.Run("interior gap", func(t *testing.T) {
		execute(t, setSequence, 23, 19)
		t.Cleanup(func() { execute(t, setSequence, 19, 23) })
		assertSelection(t, actor.UserID, cutoff, 100, domain.SyncChangePruneRange{}, domain.ErrInvalidSyncSnapshotSequence)
		assertSelection(t, actor.UserID, cutoff, 1, domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 18, Count: 1}, nil)
	})
	t.Run("unexpected end below ack limit", func(t *testing.T) {
		execute(t, setSequence, 23, 20)
		t.Cleanup(func() { execute(t, setSequence, 20, 23) })
		assertSelection(t, actor.UserID, cutoff, 100, domain.SyncChangePruneRange{}, domain.ErrInvalidSyncSnapshotSequence)
	})
	execute(t, setHighWater, 22, actor.UserID)

	// 4. 拒绝非法输入和取消，并在独立连接上核对日志仍完整。
	if prefix, err := store.selectSyncChangePrunePrefix(ctx, cutoff, 1); prefix != (domain.SyncChangePruneRange{}) || err == nil {
		t.Fatalf("未拒绝事务外调用：%+v，%v", prefix, err)
	}
	if err := store.withLockedUserTransaction(ctx, actor.UserID, true, func(txContext context.Context, txStore *Store) error {
		for _, invalid := range []struct {
			cutoff time.Time
			limit  int
		}{
			{time.Time{}, 1},
			{cutoff.Add(time.Nanosecond), 1},
			{cutoff, 0},
			{cutoff, maxSyncChangePruneBatchSize + 1},
		} {
			prefix, err := txStore.selectSyncChangePrunePrefix(txContext, invalid.cutoff, invalid.limit)
			if prefix != (domain.SyncChangePruneRange{}) || err == nil {
				return fmt.Errorf("未拒绝非法输入：%+v，%v", prefix, err)
			}
		}
		canceled, stop := context.WithCancel(txContext)
		stop()
		prefix, err := txStore.selectSyncChangePrunePrefix(canceled, cutoff, 1)
		if prefix != (domain.SyncChangePruneRange{}) || !errors.Is(err, context.Canceled) {
			return fmt.Errorf("取消查询后仍返回范围：%+v，%v", prefix, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var highWater, sequenceCount, ackPosition uint64
	err := pool.QueryRowContext(ctx, `SELECT change_seq, (SELECT COUNT(*) FROM change_events WHERE user_id=1),
		(SELECT ack_sequence FROM sync_devices WHERE device_id=19) FROM users WHERE id=1`).Scan(&highWater, &sequenceCount, &ackPosition)
	if err != nil || highWater != 22 || sequenceCount != 5 || ackPosition != 20 {
		t.Fatalf("筛选后数据库状态不符：%d，%d，%d，%v", highWater, sequenceCount, ackPosition, err)
	}
}
