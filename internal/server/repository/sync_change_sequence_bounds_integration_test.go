package repository

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestReadSyncChangeSequenceBoundsIntegration(t *testing.T) {
	// 1. 在独占库准备正式事件表和两个具有独立日志的合成账号。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	installChangeEventFixtureTable(t, ctx, pool)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(deviceDDL)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(29,2,'28ee758a-0727-42de-ae7d-3bb33155e149','Fixture','WEB')")
	execute("INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(27,2,29,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)")
	execute(`INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at) VALUES
		(19,1,UNHEX(REPEAT('11',16)),UTC_TIMESTAMP(6)),(29,2,UNHEX(REPEAT('22',16)),UTC_TIMESTAMP(6))`)
	otherActor := domain.Actor{UserID: 2, DeviceID: 29, SessionID: 27}
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(?,?,?,1,'FIXTURE_CHANGE','{}')"
	execute("UPDATE users SET change_seq=1 WHERE id=2")
	execute(insertEvent, otherActor.UserID, 1, 21)
	read := func(identity domain.Actor) (domain.SyncChangeSequenceBounds, error) {
		var bounds domain.SyncChangeSequenceBounds
		err := store.WithSyncChangePullSnapshot(ctx, identity, func(readContext context.Context, reader *Store, _ domain.SyncDeviceState) error {
			var readError error
			bounds, readError = reader.ReadSyncChangeSequenceBounds(readContext)
			return readError
		})
		return bounds, err
	}
	assertBounds := func(identity domain.Actor, minimum, highWater uint64) {
		t.Helper()
		want := domain.SyncChangeSequenceBounds{MinimumPosition: minimum, HighWaterSequence: highWater}
		if bounds, err := read(identity); err != nil || bounds != want {
			t.Fatalf("账号 %d 的边界不符：%+v，错误 %v，预期 %+v", identity.UserID, bounds, err, want)
		}
	}

	// 2. 检查初始空日志、全部裁剪、首条为 1、完整无符号上界和账号隔离。
	assertBounds(actor, 0, 0)
	assertBounds(otherActor, 0, 1)
	execute("UPDATE users SET change_seq=22 WHERE id=1")
	assertBounds(actor, 22, 22)
	for sequence := uint64(18); sequence <= 22; sequence++ {
		execute(insertEvent, actor.UserID, sequence, 11)
	}
	assertBounds(actor, 17, 22)
	execute("UPDATE users SET change_seq=? WHERE id=2", uint64(math.MaxUint64))
	execute("UPDATE change_events SET `sequence`=? WHERE user_id=2", uint64(math.MaxUint64))
	assertBounds(otherActor, math.MaxUint64-1, math.MaxUint64)
	assertBounds(actor, 17, 22)

	// 3. 并发追加和裁剪只影响随后开启的读视图。
	err := store.WithSyncChangePullSnapshot(ctx, actor, func(readContext context.Context, reader *Store, _ domain.SyncDeviceState) error {
		writer, err := pool.BeginTx(readContext, nil)
		if err != nil {
			return err
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(readContext, "DELETE FROM change_events WHERE user_id=1 AND `sequence`=18"); err != nil {
			return err
		}
		if _, err := writer.ExecContext(readContext, insertEvent, actor.UserID, 23, 11); err != nil {
			return err
		}
		if _, err := writer.ExecContext(readContext, "UPDATE users SET change_seq=23 WHERE id=1"); err != nil {
			return err
		}
		if err := writer.Commit(); err != nil {
			return err
		}
		bounds, err := reader.ReadSyncChangeSequenceBounds(readContext)
		if err != nil || bounds != (domain.SyncChangeSequenceBounds{MinimumPosition: 17, HighWaterSequence: 22}) {
			t.Fatalf("原事务混入并发更新：%+v，错误 %v", bounds, err)
		}
		canceled, stop := context.WithCancel(readContext)
		stop()
		if bounds, err := reader.ReadSyncChangeSequenceBounds(canceled); bounds != (domain.SyncChangeSequenceBounds{}) || !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后仍返回读取范围：%+v，错误 %v", bounds, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertBounds(actor, 18, 23)

	// 4. 拒绝超出上界的首条序号和缺少账号绑定的读取入口。
	execute("UPDATE users SET change_seq=18 WHERE id=1")
	if bounds, err := read(actor); bounds != (domain.SyncChangeSequenceBounds{}) || !errors.Is(err, domain.ErrInvalidSyncSnapshotSequence) {
		t.Fatalf("未拒绝矛盾的日志边界：%+v，错误 %v", bounds, err)
	}
	if bounds, err := store.ReadSyncChangeSequenceBounds(ctx); bounds != (domain.SyncChangeSequenceBounds{}) || err == nil {
		t.Fatal("普通 Store 仍返回日志区间")
	}
	err = store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
		if bounds, err := reader.ReadSyncChangeSequenceBounds(readContext); bounds != (domain.SyncChangeSequenceBounds{}) || err == nil {
			t.Fatal("未绑定账号的只读视图仍返回日志区间")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
