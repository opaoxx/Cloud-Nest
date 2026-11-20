package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithStoredObjectGCTransactionIntegration(t *testing.T) {
	// 1. 准备到期的 READY 和 ORPHANED 对象，由实际扫描产生候选。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, _ := uploadTaskDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备对象回收复核夹具失败", err)
		}
	}
	var now time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
		t.Fatal(err)
	}
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=31")
	execute("UPDATE stored_objects SET status='ORPHANED' WHERE id IN (23,24)")
	execute("UPDATE stored_objects SET unreferenced_at=?,gc_after=? WHERE id IN (23,24,29)",
		now.Add(-8*24*time.Hour), now.Add(-24*time.Hour))
	candidates, err := store.ListStoredObjectGCCandidates(ctx, 3)
	if err != nil || len(candidates) != 3 || candidates[0].ObjectID != 23 || candidates[1].ObjectID != 24 || candidates[2].ObjectID != 29 {
		t.Fatal("未取得预期的回收候选", candidates, err)
	}
	staged, disabledOwner, ready := candidates[0], candidates[1], candidates[2]
	before, err := store.FindUserStoredObject(ctx, ready.OwnerUserID, ready.ObjectID)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 回调期间用另一连接试锁用户和对象，确认两把锁都仍由事务持有。
	err = store.WithStoredObjectGCTransaction(ctx, ready, func(txContext context.Context, txStore *Store, object *domain.StoredObject) error {
		if txStore.loginUserID != ready.OwnerUserID || !reflect.DeepEqual(object, before) {
			return fmt.Errorf("回调没有收到所属用户的事务和当前对象")
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
			{"SELECT id FROM users WHERE id=? FOR UPDATE NOWAIT", ready.OwnerUserID},
			{"SELECT id FROM stored_objects WHERE id=? FOR UPDATE NOWAIT", ready.ObjectID},
		} {
			var id uint64
			var lockError *mysql.MySQLError
			err := contender.QueryRowContext(txContext, probe.statement, probe.id).Scan(&id)
			if !errors.As(err, &lockError) || lockError.Number != 3572 {
				return fmt.Errorf("回调期间行 %d 未保持排他锁：%v", probe.id, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.FindUserStoredObject(ctx, ready.OwnerUserID, ready.ObjectID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal("锁内复核改动了对象记录", after, err)
	}
	callbackFailure := errors.New("fixture rollback after GC check")
	err = store.WithStoredObjectGCTransaction(ctx, disabledOwner, func(_ context.Context, txStore *Store, object *domain.StoredObject) error {
		if txStore.loginUserID != disabledOwner.OwnerUserID || object.Status != domain.StoredObjectStatusOrphaned {
			return fmt.Errorf("未交付禁用账号的孤立对象")
		}
		return callbackFailure
	})
	if !errors.Is(err, callbackFailure) {
		t.Fatal("禁用账号的可回收对象未通过复核，或回调错误丢失", err)
	}

	// 3. 验证归属、计时、取消和状态约束会挡住失效候选。
	unexpected := func(context.Context, *Store, *domain.StoredObject) error {
		t.Error("被拒绝的候选进入回调")
		return nil
	}
	for _, scenario := range []struct {
		change func(*domain.StoredObjectGCCandidate)
		want   error
	}{
		{func(row *domain.StoredObjectGCCandidate) {
			row.OwnerUserID = disabledOwner.OwnerUserID
		}, domain.ErrStoredObjectGCCandidateStale},
		{func(row *domain.StoredObjectGCCandidate) { row.OwnerUserID = 999 }, domain.ErrAccountNotFound},
		{func(row *domain.StoredObjectGCCandidate) { row.ObjectID = 999 }, domain.ErrStoredObjectGCCandidateStale},
		{func(row *domain.StoredObjectGCCandidate) {
			row.UnreferencedAt = row.UnreferencedAt.Add(-time.Microsecond)
		}, domain.ErrStoredObjectGCCandidateStale},
		{func(row *domain.StoredObjectGCCandidate) { row.GCAfter = row.GCAfter.Add(time.Nanosecond) }, domain.ErrInvalidStoredObjectData},
	} {
		changed := ready
		scenario.change(&changed)
		if err := store.WithStoredObjectGCTransaction(ctx, changed, unexpected); !errors.Is(err, scenario.want) {
			t.Fatal("候选复核错误原因不符", changed, err)
		}
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := store.WithStoredObjectGCTransaction(canceled, ready, unexpected); !errors.Is(err, context.Canceled) {
		t.Fatal("复核未保留取消原因", err)
	}
	execute("UPDATE stored_objects SET status='DELETING' WHERE id=24")
	if err := store.WithStoredObjectGCTransaction(ctx, disabledOwner, unexpected); !errors.Is(err, domain.ErrStoredObjectGCCandidateStale) {
		t.Fatal("对象状态变化后旧候选仍有效", err)
	}
	future := staged
	future.GCAfter = now.Add(24 * time.Hour)
	execute("UPDATE stored_objects SET gc_after=? WHERE id=23", future.GCAfter)
	if err := store.WithStoredObjectGCTransaction(ctx, future, unexpected); !errors.Is(err, domain.ErrStoredObjectGCCandidateStale) {
		t.Fatal("未到期对象通过了复核", err)
	}

	// 4. 复用对象并清除计时后，旧候选失效。即使有计时，有效引用仍能挡住回收。
	err = store.withActiveUserTransaction(ctx, ready.OwnerUserID, func(txContext context.Context, txStore *Store) error {
		if err := txStore.database.WithContext(txContext).Exec(
			"UPDATE file_entries SET status='ACTIVE',object_id=?,purged_at=NULL WHERE id=31", ready.ObjectID).Error; err != nil {
			return err
		}
		_, err := txStore.ClearPublishedObjectGCTimer(txContext, ready.ObjectID, 31)
		return err
	})
	if err != nil {
		t.Fatal("恢复文件引用并清除计时失败", err)
	}
	if err := store.WithStoredObjectGCTransaction(ctx, ready, unexpected); !errors.Is(err, domain.ErrStoredObjectGCCandidateStale) {
		t.Fatal("计时清除后旧候选仍有效", err)
	}
	execute("UPDATE stored_objects SET unreferenced_at=?,gc_after=? WHERE id=29", ready.UnreferencedAt, ready.GCAfter)
	if err := store.WithStoredObjectGCTransaction(ctx, ready, unexpected); !errors.Is(err, domain.ErrStoredObjectInUse) {
		t.Fatal("仍在引用对象的文件未阻止回收", err)
	}
	execute("UPDATE stored_objects SET gc_after=? WHERE id=23", staged.GCAfter)
	execute("UPDATE upload_tasks SET status='VERIFYING' WHERE id=19")
	if err := store.WithStoredObjectGCTransaction(ctx, staged, unexpected); !errors.Is(err, domain.ErrStoredObjectInUse) {
		t.Fatal("仍在引用对象的上传任务未阻止回收", err)
	}
}
