package repository

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
	"gorm.io/gorm"
)

func TestPruneSyncChangeLogBatchIntegration(t *testing.T) {
	// 1. 两个合成账号使用重叠序号，准备保留期和设备位置形成的边界。
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
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload,created_at) " +
		"VALUES(?,?,?,1,'FIXTURE_CHANGE','{}',?)"
	execute(t, "UPDATE users SET change_seq=22 WHERE id IN (1,2)")
	for offset, createdAt := range []time.Time{older, older, older, cutoff, older} {
		execute(t, insertEvent, actor.UserID, 18+offset, 11, createdAt)
		execute(t, insertEvent, 2, 18+offset, 21, older)
	}
	execute(t, `INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at)
		VALUES(?,?,'ACTIVE',20,UNHEX(REPEAT('11',16)),?)`, actor.DeviceID, actor.UserID, cutoff)

	assertEvents := func(t *testing.T, userID uint64, want ...uint64) {
		t.Helper()
		rows, err := pool.QueryContext(ctx, "SELECT `sequence` FROM change_events WHERE user_id=? ORDER BY `sequence`", userID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []uint64
		for rows.Next() {
			var sequence uint64
			if err := rows.Scan(&sequence); err != nil {
				t.Fatal(err)
			}
			got = append(got, sequence)
		}
		if err := rows.Err(); err != nil || !slices.Equal(got, want) {
			t.Fatalf("账号 %d 的保留序号为 %v，预期 %v，错误 %v", userID, got, want, err)
		}
	}
	assertBounds := func(t *testing.T, userID, minimum uint64) {
		t.Helper()
		bounds, err := store.readSyncChangeSequenceBounds(ctx, userID)
		want := domain.SyncChangeSequenceBounds{MinimumPosition: minimum, HighWaterSequence: 22}
		if err != nil || bounds != want {
			t.Fatalf("账号 %d 的区间为 %+v，预期 %+v，错误 %v", userID, bounds, want, err)
		}
	}

	// 2. 限制测试 SQL 的实际影响行数，或在删除后取消，验证真实删除被回滚。
	t.Run("partial affected count rolls back", func(t *testing.T) {
		faultStore, err := NewStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		var affected int64
		callback := faultStore.database.Callback().Raw()
		if err := callback.Before("gorm:raw").Register("fixture:limit_sync_delete", func(tx *gorm.DB) {
			if strings.HasPrefix(tx.Statement.SQL.String(), "DELETE FROM change_events ") {
				tx.Statement.SQL.WriteString(" LIMIT 1")
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err := callback.After("gorm:raw").Register("fixture:read_sync_delete_count", func(tx *gorm.DB) {
			if strings.HasPrefix(tx.Statement.SQL.String(), "DELETE FROM change_events ") {
				affected = tx.RowsAffected
			}
		}); err != nil {
			t.Fatal(err)
		}
		got, err := faultStore.PruneSyncChangeLogBatch(ctx, actor.UserID, cutoff, 2)
		if got != (domain.SyncChangePruneRange{}) || !errors.Is(err, domain.ErrSyncChangePruneCountMismatch) || affected != 1 {
			t.Fatalf("预期实删 1 条后整批回滚：%+v，影响行数 %d，错误 %v", got, affected, err)
		}
		assertEvents(t, actor.UserID, 18, 19, 20, 21, 22)
	})
	t.Run("cancel after delete rolls back", func(t *testing.T) {
		faultStore, err := NewStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		batchContext, cancelBatch := context.WithCancel(ctx)
		defer cancelBatch()
		var affected int64
		err = faultStore.database.Callback().Raw().After("gorm:raw").Register("fixture:cancel_sync_delete", func(tx *gorm.DB) {
			if strings.HasPrefix(tx.Statement.SQL.String(), "DELETE FROM change_events ") {
				affected = tx.RowsAffected
				cancelBatch()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := faultStore.PruneSyncChangeLogBatch(batchContext, actor.UserID, cutoff, 2)
		if got != (domain.SyncChangePruneRange{}) || !errors.Is(err, context.Canceled) || affected != 2 {
			t.Fatalf("预期删除 2 条后取消回滚：%+v，影响行数 %d，错误 %v", got, affected, err)
		}
		assertEvents(t, actor.UserID, 18, 19, 20, 21, 22)
	})
	t.Run("database rejects second row", func(t *testing.T) {
		execute(t, "CREATE TRIGGER fixture_reject_sync_delete BEFORE DELETE ON change_events FOR EACH ROW "+
			"BEGIN IF OLD.user_id=1 AND OLD.`sequence`=19 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture delete failure'; END IF; END")
		t.Cleanup(func() { execute(t, "DROP TRIGGER fixture_reject_sync_delete") })
		got, err := store.PruneSyncChangeLogBatch(ctx, actor.UserID, cutoff, 2)
		var databaseError *mysql.MySQLError
		if got != (domain.SyncChangePruneRange{}) || !errors.As(err, &databaseError) || databaseError.Number != 1644 {
			t.Fatalf("数据库错误未按预期上报：%+v，%v", got, err)
		}
		assertEvents(t, actor.UserID, 18, 19, 20, 21, 22)
	})

	// 3. 正常提交只删除当前范围，后续调用从实际保留边界重新筛选。
	assertPrune := func(userID uint64, limit int, want domain.SyncChangePruneRange) {
		t.Helper()
		got, err := store.PruneSyncChangeLogBatch(ctx, userID, cutoff, limit)
		if err != nil || got != want {
			t.Fatalf("账号 %d 的裁剪范围为 %+v，预期 %+v，错误 %v", userID, got, want, err)
		}
	}
	assertPrune(actor.UserID, 2, domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 19, Count: 2})
	assertEvents(t, actor.UserID, 20, 21, 22)
	assertEvents(t, 2, 18, 19, 20, 21, 22)
	assertBounds(t, actor.UserID, 19)
	assertPrune(actor.UserID, 100, domain.SyncChangePruneRange{FirstSequence: 20, LastSequence: 20, Count: 1})
	assertEvents(t, actor.UserID, 21, 22)
	assertBounds(t, actor.UserID, 20)
	assertPrune(actor.UserID, 100, domain.SyncChangePruneRange{})

	var position uint64
	err := pool.QueryRowContext(ctx, "SELECT ack_sequence FROM sync_devices WHERE device_id=?", actor.DeviceID).Scan(&position)
	if err != nil || position != 20 {
		t.Fatalf("裁剪后确认位置改变：%d，%v", position, err)
	}
	execute(t, "UPDATE sync_devices SET ack_sequence=22 WHERE device_id=?", actor.DeviceID)
	assertPrune(actor.UserID, 100, domain.SyncChangePruneRange{})
	assertEvents(t, actor.UserID, 21, 22)
	assertEvents(t, 2, 18, 19, 20, 21, 22)

	// 没有有效设备的账号可裁剪整个已到期范围，清空日志后保留最大变更序号。
	execute(t, "UPDATE users SET status='DISABLED' WHERE id=2")
	assertPrune(2, 100, domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 22, Count: 5})
	assertEvents(t, 2)
	assertBounds(t, 2, 22)
	assertPrune(2, 100, domain.SyncChangePruneRange{})
	assertEvents(t, actor.UserID, 21, 22)
}
