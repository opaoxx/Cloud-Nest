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

func TestSettleLockedExpiredFileVersionIntegration(t *testing.T) {
	// 1. 建立到期与未到期历史以及正常文件，共享同一测试对象。
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
			t.Fatal("准备隔离结算夹具失败", err)
		}
	}
	const contentSize = 16777216
	stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
	execute("UPDATE file_entries SET revision=4 WHERE id=31")
	execute("UPDATE users SET used_storage=?,reserved_storage=19,updated_at=? WHERE id=7", 3*contentSize, stamp)
	execute("UPDATE users SET used_storage=0,reserved_storage=17,updated_at=? WHERE id=8", stamp)
	execute(`UPDATE stored_objects SET status='READY',verified_sha256=UNHEX(REPEAT('10',32)),verified_at=? WHERE id=24`, stamp)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,status,revision,purged_at)
		VALUES(32,8,14,'purged.bin',X'70','FILE','OTHER','PURGED',2,?)`, stamp)
	execute(`INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
		VALUES(51,7,31,2,29,?,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY),
			  (52,7,31,3,29,?,UTC_TIMESTAMP(6)-INTERVAL 29 DAY,UTC_TIMESTAMP(6)+INTERVAL 1 DAY),
			  (53,8,32,1,24,0,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY)`, contentSize, contentSize)
	candidates, err := store.ListExpiredFileVersionCandidates(ctx, 3)
	if err != nil || len(candidates) != 2 || candidates[0].ID != 51 || candidates[1].ID != 53 {
		t.Fatal("未取得普通和零字节到期候选", candidates, err)
	}
	candidate, empty := candidates[0], candidates[1]
	settle := func(version domain.FileVersion) error {
		return store.WithExpiredFileVersionTransaction(ctx, version,
			func(txContext context.Context, txStore *Store, current domain.FileVersion, _ *domain.StoredObject) error {
				return txStore.settleLockedExpiredFileVersion(txContext, current)
			})
	}
	type settlementState struct {
		quota, used, reserved, sequence, historyCount, events uint64
		updatedAt                                             time.Time
	}
	readState := func(userID uint64) settlementState {
		t.Helper()
		var state settlementState
		err := pool.QueryRowContext(ctx, `SELECT storage_quota,used_storage,reserved_storage,change_seq,updated_at,
			(SELECT COUNT(*) FROM file_versions WHERE user_id=users.id),
			(SELECT COUNT(*) FROM change_events WHERE user_id=users.id)
			FROM users WHERE id=?`, userID).
			Scan(&state.quota, &state.used, &state.reserved, &state.sequence, &state.updatedAt, &state.historyCount, &state.events)
		if err != nil {
			t.Fatal("读取隔离账目与历史条数失败", err)
		}
		return state
	}
	readObject := func() *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, candidate.UserID, candidate.ObjectID)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	baseState, objectBefore := readState(candidate.UserID), readObject()
	if err := store.settleLockedExpiredFileVersion(ctx, candidate); err == nil || readState(candidate.UserID) != baseState {
		t.Fatal("事务外调用未拒绝，或写入了账目和历史", err)
	}

	// 2. 已用不足及扣减语句报错时，先写入的历史删除一并回滚。
	execute("UPDATE users SET used_storage=? WHERE id=7", contentSize-1)
	shortBalance := readState(candidate.UserID)
	if err := settle(candidate); !errors.Is(err, domain.ErrInvalidAccountData) || readState(candidate.UserID) != shortBalance {
		t.Fatal("已用不足时没有保留原账目和历史行", err)
	}
	execute("UPDATE users SET used_storage=? WHERE id=7", baseState.used)
	execute(`CREATE TRIGGER fixture_fail_history_quota BEFORE UPDATE ON users
		FOR EACH ROW BEGIN
			IF OLD.id=7 AND NEW.used_storage=OLD.used_storage-16777216
				AND NOT EXISTS (SELECT 1 FROM file_versions WHERE user_id=7 AND id=51) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture history deleted before quota update';
			END IF;
		END`)
	var mysqlError *mysql.MySQLError
	if err := settle(candidate); !errors.As(err, &mysqlError) || mysqlError.Number != 1644 ||
		mysqlError.Message != "fixture history deleted before quota update" || readState(candidate.UserID) != baseState {
		t.Fatal("扣减失败未回滚已经执行的历史删除，或丢失原数据库错误", err)
	}
	execute("DROP TRIGGER fixture_fail_history_quota")

	// 3. 两次并发提交只成功一次，零字节历史也能独立结算。
	attempts := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			attempts <- settle(candidate)
		}()
	}
	close(start)
	var succeeded, stale int
	for range 2 {
		select {
		case err := <-attempts:
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, domain.ErrExpiredFileVersionCandidateStale):
				stale++
			default:
				t.Fatal("并发结算失败", err)
			}
		case <-ctx.Done():
			t.Fatal("并发结算未结束", ctx.Err())
		}
	}
	committed := readState(candidate.UserID)
	want := baseState
	want.used -= contentSize
	want.historyCount--
	want.updatedAt = committed.updatedAt
	if succeeded != 1 || stale != 1 || committed != want || committed.updatedAt.Equal(stamp) {
		t.Fatal("并发结算未恰好扣费一次", succeeded, stale, committed)
	}
	var retained uint64
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_versions
		WHERE user_id=7 AND id=52 AND revision=3 AND object_id=29 AND retained_until>UTC_TIMESTAMP(6)`).Scan(&retained)
	if err != nil || retained != 1 || !reflect.DeepEqual(readObject(), objectBefore) {
		t.Fatal("结算影响了未到期历史或共享对象", retained, err)
	}
	emptyBefore := readState(empty.UserID)
	if err := settle(empty); err != nil {
		t.Fatal("零字节历史结算失败", err)
	}
	emptyBefore.historyCount--
	if after := readState(empty.UserID); after != emptyBefore {
		t.Fatal("零字节历史结算改动了账目或更新时间", after)
	}
	t.Logf("模拟扣减失败时历史回滚；并发一项成功一项失效：已用 %d→%d B，预占 %d B；零字节历史只移除记录",
		baseState.used, committed.used, committed.reserved)
}
