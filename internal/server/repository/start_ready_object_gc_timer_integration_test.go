package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestStartReadyObjectGCTimerIntegration(t *testing.T) {
	// 1. 准备一个被文件和终态上传记录共同引用的 READY 对象。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, _ := uploadTaskDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const ownerID, readyID = 7, 29
	const allowDisabled = true
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备回收计时夹具失败", err)
		}
	}
	read := func() *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, ownerID, readyID)
		if err != nil {
			t.Fatal("读取回收计时对象失败", err)
		}
		return object
	}
	start := func(objectID uint64) (*domain.StoredObject, error) {
		var timed *domain.StoredObject
		err := store.withLockedUserTransaction(ctx, ownerID, allowDisabled, func(txContext context.Context, txStore *Store) error {
			var err error
			timed, err = txStore.StartReadyObjectGCTimer(txContext, objectID)
			return err
		})
		return timed, err
	}
	before := read()

	// 2. 校验事务、账号范围、对象状态和两类有效引用。
	if object, err := store.StartReadyObjectGCTimer(ctx, readyID); object != nil || err == nil {
		t.Fatal("普通 Store 不能启动回收计时", object, err)
	}
	for _, scenario := range []struct {
		objectID uint64
		want     error
	}{
		{24, domain.ErrStoredObjectNotFound},
		{999, domain.ErrStoredObjectNotFound},
		{23, domain.ErrInvalidStoredObjectData},
		{readyID, domain.ErrStoredObjectInUse},
	} {
		if object, err := start(scenario.objectID); object != nil || !errors.Is(err, scenario.want) {
			t.Fatal("不符合条件的对象启动了计时", scenario.objectID, object, err)
		}
	}
	execute("UPDATE file_entries SET status='PURGED', object_id=NULL, purged_at=UTC_TIMESTAMP(6) WHERE id=31")
	execute("UPDATE upload_tasks SET status='VERIFYING' WHERE id=19")
	if object, err := start(readyID); object != nil || !errors.Is(err, domain.ErrStoredObjectInUse) {
		t.Fatal("未终结任务的发布引用应保护对象", object, err)
	}
	execute("UPDATE upload_tasks SET status='COMPLETED' WHERE id=19")
	if after := read(); !reflect.DeepEqual(after, before) {
		t.Fatal("拒绝启动时对象记录发生变化", after)
	}

	// 3. 验证外层失败会回滚计时，再确认成功写入精确的七天期限。
	rollbackFixture := errors.New("fixture rolls back ready object GC timer")
	err = store.withLockedUserTransaction(ctx, ownerID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		if _, err := txStore.StartReadyObjectGCTimer(txContext, readyID); err != nil {
			return err
		}
		return rollbackFixture
	})
	if !errors.Is(err, rollbackFixture) || !reflect.DeepEqual(read(), before) {
		t.Fatal("外层事务失败后未还原对象计时", err)
	}

	var earliest, latest time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&earliest); err != nil {
		t.Fatal(err)
	}
	timed, err := start(readyID)
	if err != nil || timed == nil {
		t.Fatal("无有效引用的对象未开始计时", timed, err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if timed.UnreferencedAt == nil || timed.GCAfter == nil || timed.UnreferencedAt.Before(earliest) ||
		timed.UnreferencedAt.After(latest) || !timed.UnreferencedAt.Equal(timed.UpdatedAt) ||
		!timed.GCAfter.Equal(timed.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("无引用起点或清理期限不符", timed)
	}
	expected := *before
	expected.UnreferencedAt, expected.GCAfter, expected.UpdatedAt = timed.UnreferencedAt, timed.GCAfter, timed.UpdatedAt
	if !reflect.DeepEqual(timed, &expected) || !reflect.DeepEqual(read(), timed) {
		t.Fatal("首次计时改变了额外字段，或事务外读回不符", timed)
	}

	// 4. 重试保留已生效的计时，等待期已届满也沿用原有时间。
	if again, err := start(readyID); err != nil || !reflect.DeepEqual(again, timed) {
		t.Fatal("重试不应顺延回收期限", again, err)
	}
	elapsedStart := earliest.Add(-8 * 24 * time.Hour)
	execute("UPDATE stored_objects SET unreferenced_at=?, gc_after=?, updated_at=? WHERE id=?",
		elapsedStart, elapsedStart.Add(7*24*time.Hour), elapsedStart, readyID)
	elapsed := read()
	if again, err := start(readyID); err != nil || !reflect.DeepEqual(again, elapsed) {
		t.Fatal("已到期的计时不应重置", again, err)
	}
}
