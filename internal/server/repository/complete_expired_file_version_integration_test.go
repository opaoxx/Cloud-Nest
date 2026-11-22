package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCompleteExpiredFileVersionIntegration(t *testing.T) {
	const contentSize, reservedBytes = 16777216, 19
	for _, scenario := range []struct {
		name             string
		referringFile    bool
		referringTask    bool
		remainingHistory bool
		wantTimer        bool
	}{
		{name: "current file still refers to object", referringFile: true},
		{name: "upload task still refers to object", referringTask: true},
		{name: "another due history still refers to object", remainingHistory: true},
		{name: "last reference and timer rollback", wantTimer: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备一条到期历史，按场景在独占库中保留一种额外引用。
			pool, _ := uploadTaskDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			execute := func(statement string, args ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
					t.Fatal("准备历史与对象引用夹具失败", err)
				}
			}
			execute("UPDATE file_entries SET revision=4 WHERE id=31")
			if !scenario.referringFile {
				execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=31")
			}
			if scenario.referringTask {
				execute("UPDATE upload_tasks SET status='VERIFYING',completed_at=NULL WHERE id=19")
			}
			execute(`INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
				VALUES(51,7,31,1,29,?,UTC_TIMESTAMP(6)-INTERVAL 32 DAY,UTC_TIMESTAMP(6)-INTERVAL 2 DAY)`, contentSize)
			if scenario.remainingHistory {
				execute(`INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
					VALUES(52,7,31,2,29,?,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY)`, contentSize)
			}
			used := uint64(contentSize)
			if scenario.referringFile || scenario.remainingHistory {
				used += contentSize
			}
			execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", used, reservedBytes)
			candidates, err := store.ListExpiredFileVersionCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 || candidates[0].ID != 51 {
				t.Fatal("未取得指定的到期历史", candidates, err)
			}
			candidate := candidates[0]

			type settlementState struct {
				used, reserved, sequence, events, historyCount uint64
				updatedAt                                      time.Time
				object                                         *domain.StoredObject
			}
			readState := func() settlementState {
				t.Helper()
				var state settlementState
				err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,change_seq,updated_at,
					(SELECT COUNT(*) FROM change_events WHERE user_id=users.id),
					(SELECT COUNT(*) FROM file_versions WHERE user_id=users.id)
					FROM users WHERE id=?`, candidate.UserID).
					Scan(&state.used, &state.reserved, &state.sequence, &state.updatedAt, &state.events, &state.historyCount)
				if err != nil {
					t.Fatal("读取隔离账户与历史状态失败", err)
				}
				state.object, err = store.FindUserStoredObject(ctx, candidate.UserID, candidate.ObjectID)
				if err != nil {
					t.Fatal("读取隔离对象回收计时失败", err)
				}
				return state
			}
			before := readState()

			// 2. 只有最后一个有效引用时会尝试计时。注入计时错误以验证两项先前写入会回滚。
			if scenario.wantTimer {
				execute(`CREATE TRIGGER fixture_fail_history_gc_timer BEFORE UPDATE ON stored_objects
					FOR EACH ROW BEGIN
						IF OLD.id=29 AND NEW.gc_after IS NOT NULL
						   AND (SELECT used_storage FROM users WHERE id=7)=0
						   AND NOT EXISTS (SELECT 1 FROM file_versions WHERE user_id=7 AND id=51) THEN
							SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture history settlement reached timer';
						END IF;
					END`)
				err := store.CompleteExpiredFileVersion(ctx, candidate)
				var mysqlError *mysql.MySQLError
				if !errors.As(err, &mysqlError) || mysqlError.Number != 1644 ||
					mysqlError.Message != "fixture history settlement reached timer" || !reflect.DeepEqual(readState(), before) {
					t.Fatal("计时失败未回滚历史和已用空间，或丢失原始数据库错误", err)
				}
				execute("DROP TRIGGER fixture_fail_history_gc_timer")
			}

			// 3. 核对提交后的配额、剩余引用和计时。重放沿用原结果与对象计时。
			var earliest, latest time.Time
			if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&earliest); err != nil {
				t.Fatal(err)
			}
			if err := store.CompleteExpiredFileVersion(ctx, candidate); err != nil {
				t.Fatal("提交到期历史结算失败", err)
			}
			if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&latest); err != nil {
				t.Fatal(err)
			}
			after := readState()
			want := before
			want.used -= contentSize
			want.historyCount--
			want.updatedAt = after.updatedAt
			if scenario.wantTimer {
				started, due := after.object.UnreferencedAt, after.object.GCAfter
				if started == nil || due == nil || started.Before(earliest) || started.After(latest) || !due.Equal(started.Add(7*24*time.Hour)) {
					t.Fatal("最后引用解除后未使用数据库时间登记七天宽限期", after.object)
				}
				timed := *before.object
				timed.UnreferencedAt, timed.GCAfter, timed.UpdatedAt = started, due, *started
				want.object = &timed
			}
			if !reflect.DeepEqual(after, want) {
				t.Fatal("结算改变了预占、其他历史、事件或仍受保护的对象", after, want)
			}
			var removed uint64
			if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_versions WHERE user_id=? AND id=?", candidate.UserID, candidate.ID).
				Scan(&removed); err != nil || removed != 0 {
				t.Fatal("已结算历史仍可查到", removed, err)
			}
			if err := store.CompleteExpiredFileVersion(ctx, candidate); !errors.Is(err, domain.ErrExpiredFileVersionCandidateStale) ||
				!reflect.DeepEqual(readState(), after) {
				t.Fatal("重复候选改变了原结算结果或回收计时", err)
			}
			t.Logf("释放历史已用 %d B，预占 %d B，剩余历史 %d 条，登记七天计时 %t",
				before.used-after.used, after.reserved, after.historyCount, scenario.wantTimer)
		})
	}
}
