package repository

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateSyncSnapshotSequenceIntegration(t *testing.T) {
	// 1. 使用独占库、正式事件表和单连接用户事务，准备另一账号的事件。
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
	otherActor := domain.Actor{UserID: 2, DeviceID: 29, SessionID: 27}
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(29,2,'28ee758a-0727-42de-ae7d-3bb33155e149','Fixture','WEB')")
	execute("INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(27,2,29,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)")
	execute("UPDATE users SET change_seq=? WHERE id=2", uint64(math.MaxUint64))
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(?,?,?,1,'FIXTURE_CHANGE','{}')"
	execute(insertEvent, otherActor.UserID, uint64(math.MaxUint64), 21)
	verify := func(t *testing.T, identity domain.Actor, cutoff uint64, want error) {
		t.Helper()
		err := store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
			return txStore.ValidateSyncSnapshotSequence(txContext, cutoff)
		})
		if !errors.Is(err, want) {
			t.Fatalf("截止序号 %d 的复核错误为 %v，期望 %v", cutoff, err, want)
		}
	}

	// 2. 覆盖空库、日志已裁完、保留前缀边界及完整无符号序号。
	t.Run("empty account is isolated", func(t *testing.T) {
		verify(t, actor, 0, nil)
		verify(t, actor, 1, domain.ErrInvalidSyncSnapshotSequence)
	})
	t.Run("no retained events", func(t *testing.T) {
		execute("UPDATE users SET change_seq=22 WHERE id=1")
		verify(t, actor, 21, domain.ErrSyncSnapshotExpired)
		verify(t, actor, 22, nil)
		verify(t, actor, 23, domain.ErrInvalidSyncSnapshotSequence)
	})
	t.Run("retained contiguous suffix", func(t *testing.T) {
		for sequence := uint64(18); sequence <= 22; sequence++ {
			execute(insertEvent, actor.UserID, sequence, 11)
		}
		verify(t, actor, 16, domain.ErrSyncSnapshotExpired)
		verify(t, actor, 17, nil)
		verify(t, actor, 20, nil)
		verify(t, actor, 22, nil)
		verify(t, actor, 23, domain.ErrInvalidSyncSnapshotSequence)
		var sequence, eventCount uint64
		if err := pool.QueryRowContext(ctx, "SELECT change_seq, (SELECT COUNT(*) FROM change_events WHERE user_id=1) FROM users WHERE id=1").
			Scan(&sequence, &eventCount); err != nil || sequence != 22 || eventCount != 5 {
			t.Fatalf("只读复核后账号序号=%d，事件数=%d，错误=%v", sequence, eventCount, err)
		}
	})
	t.Run("full unsigned range", func(t *testing.T) {
		verify(t, otherActor, math.MaxUint64-2, domain.ErrSyncSnapshotExpired)
		verify(t, otherActor, math.MaxUint64-1, nil)
		verify(t, otherActor, math.MaxUint64, nil)
	})
	t.Run("inconsistent first retained event", func(t *testing.T) {
		execute("UPDATE users SET change_seq=17 WHERE id=1")
		verify(t, actor, 17, domain.ErrInvalidSyncSnapshotSequence)
	})

	// 3. 确认普通 Store、缺少认证会话的事务及已取消调用无法复核。
	t.Run("requires authenticated transaction", func(t *testing.T) {
		if err := store.ValidateSyncSnapshotSequence(ctx, 0); err == nil {
			t.Fatal("普通 Store 通过了复核")
		}
		err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
			return txStore.ValidateSyncSnapshotSequence(txContext, 0)
		})
		if err == nil {
			t.Fatal("缺少认证会话的账号事务通过了复核")
		}
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			canceled, stop := context.WithCancel(txContext)
			stop()
			return txStore.ValidateSyncSnapshotSequence(canceled, 22)
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后的复核返回 %v", err)
		}
	})
}
