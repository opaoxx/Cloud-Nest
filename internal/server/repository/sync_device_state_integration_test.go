package repository

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestFindCurrentSyncDeviceStateIntegration(t *testing.T) {
	// 1. 在独占库加载正式的同步设备表，并为两个账号准备合成设备和会话。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到第 3 版同步设备表定义")
	}
	execute := func(t *testing.T, queryContext context.Context, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(queryContext, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, ctx, deviceDDL)
	execute(t, ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(29,2,'28ee758a-0727-42de-ae7d-3bb33155e149','Fixture','WEB')")
	execute(t, ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(27,2,29,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)")
	otherActor := domain.Actor{UserID: 2, DeviceID: 29, SessionID: 27}
	generation := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	otherGeneration := generation
	otherGeneration[15] ^= 1
	execute(t, ctx, `INSERT INTO sync_devices(device_id,user_id,ack_sequence,status,cursor_generation,last_seen_at)
		VALUES (29,2,?,'ACTIVE',?,UTC_TIMESTAMP(6))`, uint64(math.MaxUint64), otherGeneration[:])
	read := func(identity domain.Actor) (state domain.SyncDeviceState, err error) {
		err = store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
			state, err = txStore.FindCurrentSyncDeviceState(txContext)
			return err
		})
		return state, err
	}

	// 2. 核对缺失、账号隔离、待确认和活动记录，比较完整记录确认只读。
	if state, err := read(actor); !errors.Is(err, domain.ErrSyncDeviceNotFound) || state != (domain.SyncDeviceState{}) {
		t.Fatalf("缺失记录错误：状态 %+v，错误 %v", state, err)
	}
	execute(t, ctx, `INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at)
		VALUES (19,1,?,UTC_TIMESTAMP(6))`, generation[:])
	fingerprint := func(t *testing.T, deviceID uint64) string {
		t.Helper()
		var contents string
		err := pool.QueryRowContext(ctx, `SELECT CONCAT_WS('|',device_id,user_id,status,HEX(cursor_generation),ack_sequence,last_seen_at,created_at,updated_at)
			FROM sync_devices WHERE device_id=?`, deviceID).Scan(&contents)
		if err != nil {
			t.Fatal(err)
		}
		return contents
	}
	for _, testCase := range []struct {
		name  string
		actor domain.Actor
		want  domain.SyncDeviceState
	}{
		{"pending with full generation", actor, domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: generation}},
		{"other account with maximum unsigned position", otherActor, domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: otherGeneration, AckSequence: math.MaxUint64}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			before := fingerprint(t, testCase.actor.DeviceID)
			state, err := read(testCase.actor)
			if err != nil || state != testCase.want || fingerprint(t, testCase.actor.DeviceID) != before {
				t.Fatalf("锁读状态不符或修改了记录：状态 %+v，错误 %v", state, err)
			}
		})
	}
	wrongOwner := actor
	wrongOwner.DeviceID = otherActor.DeviceID
	if state, err := read(wrongOwner); !errors.Is(err, domain.ErrSessionNotFound) || state != (domain.SyncDeviceState{}) {
		t.Fatalf("跨账号读取未被拒绝：状态 %+v，错误 %v", state, err)
	}

	// 3. 核对锁读绕过旧读视图，并实际锁住目标同步行直到事务结束。
	t.Run("current read holds row lock", func(t *testing.T) {
		const plainRead = "SELECT ack_sequence FROM sync_devices WHERE device_id = ?"
		const nowaitRead = "SELECT ack_sequence FROM sync_devices WHERE device_id = ? FOR UPDATE NOWAIT"
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var oldPosition uint64
			if err := txStore.database.WithContext(txContext).Raw(plainRead, actor.DeviceID).Row().Scan(&oldPosition); err != nil {
				return err
			}
			// 第二条连接仅在夹具中修改同步行，制造同一事务里的旧视图。
			execute(t, txContext, "UPDATE sync_devices SET status='ACTIVE',ack_sequence=17,cursor_generation=? WHERE device_id=?",
				otherGeneration[:], actor.DeviceID)
			var consistentPosition uint64
			if err := txStore.database.WithContext(txContext).Raw(plainRead, actor.DeviceID).Row().Scan(&consistentPosition); err != nil {
				return err
			}
			if oldPosition != 0 || consistentPosition != 0 {
				t.Fatalf("夹具未保留原一致性视图：%d, %d", oldPosition, consistentPosition)
			}
			state, err := txStore.FindCurrentSyncDeviceState(txContext)
			if err != nil || state != (domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: otherGeneration, AckSequence: 17}) {
				t.Fatalf("未锁读到当前已提交状态：%+v，错误 %v", state, err)
			}
			contender, err := pool.BeginTx(txContext, nil)
			if err != nil {
				return err
			}
			defer contender.Rollback()
			var position uint64
			err = contender.QueryRowContext(txContext, nowaitRead, actor.DeviceID).Scan(&position)
			var lockError *mysql.MySQLError
			if !errors.As(err, &lockError) || lockError.Number != 3572 {
				t.Fatalf("独立连接应遇到同步行锁：%v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		var position uint64
		if err := pool.QueryRowContext(ctx, nowaitRead, actor.DeviceID).Scan(&position); err != nil || position != 17 {
			t.Fatalf("事务结束后行锁未释放或位置不符：%d，错误 %v", position, err)
		}
	})

	// 4. 检查入口限制、取消和异常记录，所有失败均交付空结果。
	t.Run("rejects invalid transaction and state", func(t *testing.T) {
		if state, err := store.FindCurrentSyncDeviceState(ctx); err == nil || state != (domain.SyncDeviceState{}) {
			t.Fatal("普通 Store 读出了确认状态")
		}
		err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
			_, err := txStore.FindCurrentSyncDeviceState(txContext)
			return err
		})
		if err == nil {
			t.Fatal("未经会话复核的事务读出了确认状态")
		}
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			canceled, stop := context.WithCancel(txContext)
			stop()
			state, err := txStore.FindCurrentSyncDeviceState(canceled)
			if state != (domain.SyncDeviceState{}) {
				t.Fatal("已取消的调用读出了确认状态")
			}
			return err
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消错误不符：%v", err)
		}
		for _, mutation := range []string{
			"UPDATE sync_devices SET status='RESET_REQUIRED' WHERE device_id=19",
			"UPDATE sync_devices SET status='ACTIVE',cursor_generation=UNHEX(REPEAT('00',16)) WHERE device_id=19",
		} {
			execute(t, ctx, mutation)
			if state, err := read(actor); !errors.Is(err, domain.ErrInvalidSyncDeviceData) || state != (domain.SyncDeviceState{}) {
				t.Fatalf("异常状态未被拒绝：%+v，错误 %v", state, err)
			}
		}
	})
}
