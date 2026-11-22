package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithExpiredFileVersionTransactionIntegration(t *testing.T) {
	// 1. 复用正式历史表和对象夹具，从真实扫描取得两个账号的候选。
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备到期历史复核夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
		t.Fatal(err)
	}
	createdAt, deadline := now.AddDate(0, 0, -31), now.AddDate(0, 0, -1)
	execute(t, "UPDATE users SET used_storage=33554432,reserved_storage=19 WHERE id=7")
	execute(t, "UPDATE file_entries SET revision=6 WHERE id=31")
	execute(t, `UPDATE stored_objects SET status='READY',verified_sha256=UNHEX(REPEAT('10',32)),verified_at=? WHERE id=24`, createdAt)
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,status,purged_at)
		VALUES(32,8,14,'purged.bin',X'70','FILE','OTHER','PURGED',?)`, deadline)
	execute(t, `INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
		VALUES(51,7,31,4,29,16777216,?,?),(52,8,32,1,24,0,?,?)`, createdAt, deadline, createdAt, deadline)
	candidates, err := store.ListExpiredFileVersionCandidates(ctx, 2)
	if err != nil || len(candidates) != 2 || candidates[0].ID != 51 || candidates[1].ID != 52 {
		t.Fatal("扫描未交付预期的历史候选", candidates, err)
	}
	candidate, disabled := candidates[0], candidates[1]

	// 2. 回调期间，另一连接不能取得账号、对象和历史行的排他锁。
	readAccounting := func() string {
		t.Helper()
		var data string
		err := pool.QueryRowContext(ctx, `SELECT JSON_ARRAY(account.used_storage,account.reserved_storage,account.updated_at,
			object.status,object.unreferenced_at,object.gc_after,object.updated_at)
			FROM users account JOIN stored_objects object ON object.owner_user_id=account.id
			WHERE account.id=? AND object.id=?`, candidate.UserID, candidate.ObjectID).Scan(&data)
		if err != nil {
			t.Fatal("读取隔离配额及对象计时失败", err)
		}
		return data
	}
	accountingBefore := readAccounting()
	err = store.WithExpiredFileVersionTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, current domain.FileVersion, object *domain.StoredObject) error {
		if txStore.loginUserID != candidate.UserID || current != candidate || object.ID != candidate.ObjectID || object.FileSize != candidate.FileSize {
			return fmt.Errorf("回调未收到同一账号事务及对应历史和对象")
		}
		contender, err := pool.BeginTx(txContext, nil)
		if err != nil {
			return err
		}
		defer contender.Rollback()
		for _, probe := range []struct {
			statement string
			id        uint64
		}{
			{"SELECT id FROM users WHERE id=? FOR UPDATE NOWAIT", candidate.UserID},
			{"SELECT id FROM stored_objects WHERE id=? FOR UPDATE NOWAIT", candidate.ObjectID},
			{"SELECT id FROM file_versions WHERE id=? FOR UPDATE NOWAIT", candidate.ID},
		} {
			var id uint64
			var lockError *mysql.MySQLError
			err := contender.QueryRowContext(txContext, probe.statement, probe.id).Scan(&id)
			if !errors.As(err, &lockError) || lockError.Number != 3572 {
				return fmt.Errorf("回调期间行 %d 未持有排他锁: %v", probe.id, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var unlockedID uint64
	if err := pool.QueryRowContext(ctx, "SELECT id FROM file_versions WHERE id=? FOR UPDATE NOWAIT", candidate.ID).
		Scan(&unlockedID); err != nil || unlockedID != candidate.ID {
		t.Fatal("事务结束后历史行锁未释放", err)
	}
	callbackFailure := errors.New("fixture stops after expired history recheck")
	err = store.WithExpiredFileVersionTransaction(ctx, disabled, func(_ context.Context, txStore *Store, current domain.FileVersion, object *domain.StoredObject) error {
		if txStore.loginUserID != disabled.UserID || current != disabled || current.FileSize != 0 || object.ID != disabled.ObjectID {
			return fmt.Errorf("禁用账号的零字节历史未按归属交付")
		}
		return callbackFailure
	})
	if !errors.Is(err, callbackFailure) {
		t.Fatal("禁用账号的候选未通过，或回调错误未保留", err)
	}
	if after, err := store.ListExpiredFileVersionCandidates(ctx, 2); err != nil || len(after) != 2 ||
		after[0] != candidate || after[1] != disabled || readAccounting() != accountingBefore {
		t.Fatal("复核改变了历史、配额或对象回收计时", after, err)
	}

	// 3. 模拟扫描后记录变化和移除，旧快照均不能进入回调。
	unexpected := func(context.Context, *Store, domain.FileVersion, *domain.StoredObject) error {
		t.Error("被拒绝的候选进入了回调")
		return nil
	}
	checkRejected := func(input domain.FileVersion, want error) {
		t.Helper()
		if err := store.WithExpiredFileVersionTransaction(ctx, input, unexpected); !errors.Is(err, want) {
			t.Fatal("候选复核返回了意外错误", input, err)
		}
	}
	foreignObject := candidate
	foreignObject.ObjectID = disabled.ObjectID
	checkRejected(foreignObject, domain.ErrExpiredFileVersionCandidateStale)
	foreignHistory := candidate
	foreignHistory.ID = disabled.ID
	checkRejected(foreignHistory, domain.ErrExpiredFileVersionCandidateStale)
	invalid := candidate
	invalid.RetainedUntil = invalid.RetainedUntil.Add(time.Nanosecond)
	checkRejected(invalid, domain.ErrInvalidFileVersionData)
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if err := store.WithExpiredFileVersionTransaction(cancelled, candidate, unexpected); !errors.Is(err, context.Canceled) {
		t.Fatal("复核未保留取消原因", err)
	}

	execute(t, "UPDATE file_versions SET revision=5 WHERE id=51")
	checkRejected(candidate, domain.ErrExpiredFileVersionCandidateStale)
	execute(t, "UPDATE file_versions SET revision=?,retained_until=? WHERE id=51", candidate.Revision, deadline.Add(time.Microsecond))
	checkRejected(candidate, domain.ErrExpiredFileVersionCandidateStale)
	future := candidate
	future.RetainedUntil = now.Add(time.Hour)
	execute(t, "UPDATE file_versions SET retained_until=? WHERE id=51", future.RetainedUntil)
	checkRejected(future, domain.ErrExpiredFileVersionCandidateStale)
	execute(t, "UPDATE file_versions SET retained_until=?,file_size=file_size+1 WHERE id=51", deadline)
	checkRejected(candidate, domain.ErrExpiredFileVersionCandidateStale)

	execute(t, "UPDATE file_versions SET file_size=? WHERE id=51", candidate.FileSize)
	execute(t, "UPDATE stored_objects SET status='ORPHANED',unreferenced_at=?,gc_after=? WHERE id=29", now, now.AddDate(0, 0, 7))
	checkRejected(candidate, domain.ErrInvalidStoredObjectData)
	execute(t, "UPDATE stored_objects SET status='READY',unreferenced_at=NULL,gc_after=NULL WHERE id=29")
	// 仅夹具删除历史行，模拟它在扫描后已被另一事务结算。
	execute(t, "DELETE FROM file_versions WHERE user_id=? AND id=?", candidate.UserID, candidate.ID)
	checkRejected(candidate, domain.ErrExpiredFileVersionCandidateStale)
	t.Log("账号、对象、历史行的三次 NOWAIT 试锁均被拒绝；快照变化、未来截止、跨账号和已移除候选均未进入回调")
}
