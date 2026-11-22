package repository

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestExpiredFileVersionStoredObjectGCRaceIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	// 两个业务事务、一个闸门事务和一条观察连接各占一条连接。
	pool.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("设置隔离并发夹具失败", err)
		}
	}
	const userID, objectID, contentBytes, reservedBytes = 7, 29, 16777216, 19
	readObject := func() *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, userID, objectID)
		if err != nil {
			t.Fatal("读取隔离对象状态失败", err)
		}
		return object
	}

	// 1. 先扫描实际的旧回收计时，随后模拟仍保有历史引用时复核旧候选。
	execute("UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6),revision=3 WHERE id=31")
	execute("UPDATE stored_objects SET unreferenced_at=UTC_TIMESTAMP(6)-INTERVAL 9 DAY,gc_after=UTC_TIMESTAMP(6)-INTERVAL 2 DAY WHERE id=29")
	oldCandidates, err := store.ListStoredObjectGCCandidates(ctx, 1)
	if err != nil || len(oldCandidates) != 1 || oldCandidates[0].ObjectID != objectID {
		t.Fatal("未读取测试对象的旧回收候选", oldCandidates, err)
	}
	oldCandidate := oldCandidates[0]
	execute(`INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
		VALUES(51,7,31,1,29,?,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY)`, contentBytes)
	execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", contentBytes, reservedBytes)
	if object, err := store.ClaimStoredObjectGC(ctx, oldCandidate); object != nil || !errors.Is(err, domain.ErrStoredObjectInUse) {
		t.Fatal("仍有历史引用的对象被领取，或未返回引用保护错误", err)
	}
	execute("UPDATE stored_objects SET unreferenced_at=NULL,gc_after=NULL WHERE id=29")
	history, err := store.ListExpiredFileVersionCandidates(ctx, 1)
	if err != nil || len(history) != 1 || history[0].ID != 51 {
		t.Fatal("未扫描到指定历史版本", history, err)
	}

	// 2. 让正式结算在计时写入前等待闸门，观察回收领取实际等待同一账号锁。
	execute("CREATE TABLE fixture_history_gc_gate (id INT PRIMARY KEY) ENGINE=InnoDB")
	execute("INSERT INTO fixture_history_gc_gate VALUES (1)")
	execute(`CREATE TRIGGER fixture_pause_history_gc_timer BEFORE UPDATE ON stored_objects
		FOR EACH ROW BEGIN
			DECLARE gate_id INT;
			IF OLD.id=29 AND OLD.gc_after IS NULL AND NEW.gc_after IS NOT NULL THEN
				SELECT id INTO gate_id FROM fixture_history_gc_gate WHERE id=1 FOR UPDATE;
			END IF;
		END`)
	gate, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	defer func() {
		_ = gate.Rollback()
		cancel()
		workers.Wait()
	}()
	var gateID int
	if err := gate.QueryRowContext(ctx, "SELECT id FROM fixture_history_gc_gate WHERE id=1 FOR UPDATE").Scan(&gateID); err != nil {
		t.Fatal("无法持有隔离闸门行锁", err)
	}

	waitForLock := func(table string) {
		t.Helper()
		waitContext, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		poll := time.NewTicker(10 * time.Millisecond)
		defer poll.Stop()
		for {
			var waiting bool
			err := pool.QueryRowContext(waitContext, `SELECT EXISTS (
				SELECT 1 FROM performance_schema.data_lock_waits waits
				JOIN performance_schema.data_locks requested
					ON requested.ENGINE=waits.ENGINE AND requested.ENGINE_LOCK_ID=waits.REQUESTING_ENGINE_LOCK_ID
				WHERE requested.OBJECT_SCHEMA=DATABASE() AND requested.OBJECT_NAME=?)`, table).Scan(&waiting)
			if err != nil {
				t.Fatal("读取隔离数据库锁等待失败", table, err)
			}
			if waiting {
				return
			}
			select {
			case <-waitContext.Done():
				t.Fatal("未观察到约定的行锁等待", table, waitContext.Err())
			case <-poll.C:
			}
		}
	}

	settled := make(chan error, 1)
	workers.Go(func() { settled <- store.CompleteExpiredFileVersion(ctx, history[0]) })
	waitForLock("fixture_history_gc_gate")
	var used, reserved, retained uint64
	readAccounting := func() {
		t.Helper()
		err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
			(SELECT COUNT(*) FROM file_versions WHERE user_id=users.id AND id=51)
			FROM users WHERE id=7`).Scan(&used, &reserved, &retained)
		if err != nil {
			t.Fatal("读取隔离历史与账目失败", err)
		}
	}
	readAccounting()
	if used != contentBytes || reserved != reservedBytes || retained != 1 || readObject().GCAfter != nil {
		t.Fatal("并发连接看到了未提交的历史结算或对象计时", used, reserved, retained)
	}

	type claimResult struct {
		object *domain.StoredObject
		err    error
	}
	claimed := make(chan claimResult, 1)
	workers.Go(func() {
		object, err := store.ClaimStoredObjectGC(ctx, oldCandidate)
		claimed <- claimResult{object, err}
	})
	waitForLock("users")
	if err := gate.Rollback(); err != nil {
		t.Fatal("放行隔离结算事务失败", err)
	}
	if err := <-settled; err != nil {
		t.Fatal("历史结算未成功提交", err)
	}
	if claim := <-claimed; claim.object != nil || !errors.Is(claim.err, domain.ErrStoredObjectGCCandidateStale) {
		t.Fatal("并发领取未按新计时拒绝旧候选", claim.err)
	}
	workers.Wait()
	execute("DROP TRIGGER fixture_pause_history_gc_timer")

	// 3. 用独占连接时钟核对七天边界，再通过正式扫描领取一份新候选。
	timed := readObject()
	readAccounting()
	if used != 0 || reserved != reservedBytes || retained != 0 || timed.Status != domain.StoredObjectStatusReady ||
		timed.UnreferencedAt == nil || timed.GCAfter == nil || !timed.GCAfter.Equal(timed.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("提交后的账目、历史或七天计时不符", used, reserved, retained, timed)
	}
	pool.SetMaxIdleConns(1)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := pool.ExecContext(cleanup, "SET timestamp = DEFAULT"); err != nil {
			t.Error("恢复独占连接时钟失败", err)
		}
	})
	execute("SET timestamp = ?", timed.GCAfter.Unix()-1)
	if candidates, err := store.ListStoredObjectGCCandidates(ctx, 1); err != nil || len(candidates) != 0 {
		t.Fatal("七天宽限期内对象提前出现在回收扫描中", candidates, err)
	}
	execute("SET timestamp = ?", timed.GCAfter.Unix()+1)
	if object, err := store.ClaimStoredObjectGC(ctx, oldCandidate); object != nil ||
		!errors.Is(err, domain.ErrStoredObjectGCCandidateStale) || !reflect.DeepEqual(readObject(), timed) {
		t.Fatal("即使当前计时到期，旧计时也不得作为领取依据", err)
	}
	fresh, err := store.ListStoredObjectGCCandidates(ctx, 1)
	if err != nil || len(fresh) != 1 || fresh[0].ObjectID != objectID ||
		!fresh[0].UnreferencedAt.Equal(*timed.UnreferencedAt) || !fresh[0].GCAfter.Equal(*timed.GCAfter) {
		t.Fatal("到期后未扫描到绑定新计时的唯一候选", fresh, err)
	}
	intent, err := store.ClaimStoredObjectGC(ctx, fresh[0])
	if err != nil || intent == nil || intent.ID != objectID || intent.Status != domain.StoredObjectStatusDeleting {
		t.Fatal("七天后未取得该对象的删除意图", err)
	}
	t.Logf("历史保护挡住旧候选，并发领取等待同一账号锁；结算后已用 %d B、预占 %d B，计时精确七天，仅新候选可领取删除意图", used, reserved)
}
