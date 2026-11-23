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

func TestWithSyncChangePullSnapshotIntegration(t *testing.T) {
	// 1. 在独占库安装正式同步表，为两名合成用户准备记录。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, deviceDDL)
	otherActor := domain.Actor{UserID: 2, DeviceID: 29, SessionID: 27}
	execute(t, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(29,2,'28ee758a-0727-42de-ae7d-3bb33155e149','Fixture','WEB')")
	execute(t, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(27,2,29,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)")
	generation := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	otherGeneration := generation
	otherGeneration[15] ^= 1
	execute(t, "UPDATE users SET change_seq=? WHERE id=2", uint64(math.MaxUint64))
	execute(t, "INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at) VALUES(29,2,'ACTIVE',?,?,'2020-01-01')",
		uint64(math.MaxUint64), otherGeneration[:])
	read := func(callContext context.Context, identity domain.Actor) (domain.SyncDeviceState, error) {
		var state domain.SyncDeviceState
		err := store.WithSyncChangePullSnapshot(callContext, identity, func(_ context.Context, _ *Store, found domain.SyncDeviceState) error {
			state = found
			return nil
		})
		return state, err
	}
	assertRejected := func(t *testing.T, callContext context.Context, identity domain.Actor, expected error) {
		t.Helper()
		if state, err := read(callContext, identity); !errors.Is(err, expected) || state != (domain.SyncDeviceState{}) {
			t.Fatalf("应拒绝读取：状态 %+v，错误 %v，预期 %v", state, err, expected)
		}
	}
	assertRejected(t, ctx, actor, domain.ErrSyncDeviceNotFound)
	wrongDevice := actor
	wrongDevice.DeviceID = otherActor.DeviceID
	assertRejected(t, ctx, wrongDevice, domain.ErrSessionNotFound)
	wrongSession := actor
	wrongSession.SessionID = otherActor.SessionID
	assertRejected(t, ctx, wrongSession, domain.ErrSessionNotFound)
	otherState := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: otherGeneration, AckSequence: math.MaxUint64}
	if state, err := read(ctx, otherActor); err != nil || state != otherState {
		t.Fatalf("另一账号只应读到自己的完整状态：%+v，错误 %v", state, err)
	}
	execute(t, "INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at,updated_at) VALUES(19,1,?,'2020-01-01','2020-01-02')", generation[:])
	pending := domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: generation}
	if state, err := read(ctx, actor); err != nil || state != pending {
		t.Fatalf("待确认设备状态不符：%+v，错误 %v", state, err)
	}
	var unchangedTimes int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_devices WHERE device_id=19
		AND last_seen_at='2020-01-01' AND updated_at='2020-01-02'`).Scan(&unchangedTimes); err != nil || unchangedTimes != 1 {
		t.Fatalf("只读操作改写了时间：%v", err)
	}

	// 2. 第二条连接提交新的同步状态，本次一致性视图仍保持旧数据。
	active := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: otherGeneration, AckSequence: 17}
	t.Run("same read view tolerates concurrent commit", func(t *testing.T) {
		err := store.WithSyncChangePullSnapshot(ctx, actor, func(readContext context.Context, reader *Store, state domain.SyncDeviceState) error {
			if state != pending {
				t.Fatalf("初次读取不符：%+v", state)
			}
			if _, err := pool.ExecContext(readContext, "UPDATE sync_devices SET status='ACTIVE',ack_sequence=17,cursor_generation=? WHERE device_id=19", otherGeneration[:]); err != nil {
				return err
			}
			state, err := reader.readSyncDeviceState(readContext, actor.UserID, actor.DeviceID, false)
			if err != nil || state != pending {
				t.Fatalf("原事务混入并发更新：%+v，错误 %v", state, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if state, err := read(ctx, actor); err != nil || state != active {
			t.Fatalf("新事务未读到已提交的状态：%+v，错误 %v", state, err)
		}
	})

	// 3. 认证变化、已取消请求和无效状态均在回调前拒绝。
	for _, testCase := range []struct {
		name, prepare, restore string
		want                   error
	}{
		{"disabled account", "UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
		{"revoked device", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=19", "UPDATE user_devices SET revoked_at=NULL WHERE id=19", domain.ErrDeviceRevoked},
		{"revoked session", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		{"expired session", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
		{"nonzero pending position", "UPDATE sync_devices SET status='RESET_REQUIRED' WHERE device_id=19", "UPDATE sync_devices SET status='ACTIVE' WHERE device_id=19", domain.ErrInvalidSyncDeviceData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			execute(t, testCase.prepare)
			defer execute(t, testCase.restore)
			assertRejected(t, ctx, actor, testCase.want)
		})
	}
	missingSession := actor
	missingSession.SessionID = 0
	assertRejected(t, ctx, missingSession, domain.ErrInvalidSessionData)
	canceledContext, stop := context.WithCancel(ctx)
	stop()
	assertRejected(t, canceledContext, actor, context.Canceled)
	if state, err := read(ctx, actor); err != nil || state != active {
		t.Fatalf("拒绝场景未保持最终同步状态：%+v，错误 %v", state, err)
	}
}
