package repository

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRenameUserDeviceIntegration(t *testing.T) {
	// 1. 复用独占库和正式设备/会话表，建立事务所需的有效身份。
	pool := loginSessionDatabase(t)
	pool.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	newActor := func(t *testing.T, userID uint64, installation string) domain.Actor {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installation, "原设备", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		})
		if err != nil || session == nil {
			t.Fatal("创建改名测试会话失败", err)
		}
		return domain.Actor{UserID: userID, DeviceID: session.DeviceID, SessionID: session.ID}
	}
	type snapshot struct {
		installation, name, platform string
		createdAt, updatedAt         time.Time
		lastSeenAt, revokedAt        sql.NullTime
		osVersion, appVersion        sql.NullString
	}
	read := func(t *testing.T, deviceID uint64) snapshot {
		t.Helper()
		var value snapshot
		err := pool.QueryRowContext(ctx, "SELECT installation_id,name,platform,created_at,updated_at,last_seen_at,revoked_at,os_version,app_version FROM user_devices WHERE id=?", deviceID).
			Scan(&value.installation, &value.name, &value.platform, &value.createdAt, &value.updatedAt, &value.lastSeenAt, &value.revokedAt, &value.osVersion, &value.appVersion)
		if err != nil {
			t.Fatal("读取设备快照失败", err)
		}
		return value
	}
	rename := func(actor domain.Actor, targetID uint64, name string) (*domain.UserDevice, error) {
		var tentative *domain.UserDevice
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var err error
			tentative, err = txStore.RenameUserDevice(txContext, targetID, name)
			return err
		})
		if err != nil {
			return nil, err
		}
		return tentative, nil
	}
	// 2. 核对允许修改的字段、相同名称、已撤销目标及账号隔离。
	t.Run("scope fields current and revoked targets", func(t *testing.T) {
		installation := uuid.NewString()
		caller := newActor(t, 1, installation)
		sameDevice := newActor(t, 1, installation)
		target := newActor(t, 1, uuid.NewString())
		foreign := newActor(t, 3, installation)
		baselineTime := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET last_seen_at=?,revoked_at=?,updated_at=?,os_version='学习系统',app_version='1.0' WHERE id=?", baselineTime, baselineTime, baselineTime, target.DeviceID); err != nil {
			t.Fatal(err)
		}
		original, callerBefore, foreignBefore := read(t, target.DeviceID), read(t, caller.DeviceID), read(t, foreign.DeviceID)
		var sessionStateBefore string
		if err := pool.QueryRowContext(ctx, "SELECT CONCAT(HEX(refresh_token_hash),':',token_generation,':',expires_at,':',updated_at,':',IFNULL(revoked_at,'')) FROM auth_sessions WHERE id=?", sameDevice.SessionID).Scan(&sessionStateBefore); err != nil {
			t.Fatal(err)
		}
		name := "  学习电脑 🎮  "
		result, err := rename(caller, target.DeviceID, name)
		if err != nil || result == nil || result.ID != target.DeviceID || result.UserID != caller.UserID || result.Name != name || result.RevokedAt == nil || !result.RevokedAt.Equal(baselineTime) {
			t.Fatal("目标名称或撤销事实错误", err)
		}
		committed := read(t, target.DeviceID)
		expected := original
		expected.name, expected.updatedAt = name, committed.updatedAt
		if !reflect.DeepEqual(committed, expected) || !committed.updatedAt.After(baselineTime) || committed.updatedAt.Nanosecond()%1000 != 0 {
			t.Fatal("设备字段或数据库更新时间错误")
		}
		for attempt := 0; attempt < 2; attempt++ {
			if _, err := rename(caller, target.DeviceID, name); err != nil || !reflect.DeepEqual(read(t, target.DeviceID), committed) {
				t.Fatal("同名重试改变了设备事实", err)
			}
		}
		if _, err := rename(sameDevice, caller.DeviceID, "当前学习设备"); err != nil {
			t.Fatal("当前设备改名失败", err)
		}
		var sessionStateAfter string
		if err := pool.QueryRowContext(ctx, "SELECT CONCAT(HEX(refresh_token_hash),':',token_generation,':',expires_at,':',updated_at,':',IFNULL(revoked_at,'')) FROM auth_sessions WHERE id=?", sameDevice.SessionID).Scan(&sessionStateAfter); err != nil || sessionStateAfter != sessionStateBefore {
			t.Fatal("改名影响了同设备登录会话", err)
		}
		for _, targetID := range []uint64{foreign.DeviceID, ^uint64(0)} {
			if result, err := rename(caller, targetID, "其他名称"); result != nil || !errors.Is(err, domain.ErrDeviceNotFound) {
				t.Fatal("其他账号与不存在目标未统一处理", err)
			}
		}
		if !reflect.DeepEqual(read(t, foreign.DeviceID), foreignBefore) || callerBefore.installation != read(t, caller.DeviceID).installation {
			t.Fatal("其他设备或安装标识被修改")
		}
		if _, err := rename(caller, caller.DeviceID, strings.Repeat("🎮", 100)); err != nil {
			t.Fatal("100 个 emoji 的名称写入失败", err)
		}
	})
	t.Run("error panic and cancellation rollback", func(t *testing.T) {
		caller, target := newActor(t, 1, uuid.NewString()), newActor(t, 1, uuid.NewString())
		baseline := read(t, target.DeviceID)
		failure := errors.New("callback failed")
		for _, mode := range []string{"error", "panic", "cancel"} {
			t.Run(mode, func(t *testing.T) {
				requestContext, stop := context.WithCancel(ctx)
				defer stop()
				var recovered any
				var operationError error
				func() {
					defer func() { recovered = recover() }()
					operationError = store.WithUserTransaction(requestContext, caller, func(txContext context.Context, txStore *Store) error {
						if _, err := txStore.RenameUserDevice(txContext, target.DeviceID, "回滚名称"); err != nil {
							return err
						}
						switch mode {
						case "error":
							return failure
						case "panic":
							panic("rename callback panic")
						default:
							stop()
							return nil
						}
					})
				}()
				if (mode == "error" && !errors.Is(operationError, failure)) || (mode == "panic" && recovered == nil) || (mode == "cancel" && !errors.Is(operationError, context.Canceled)) || !reflect.DeepEqual(read(t, target.DeviceID), baseline) {
					t.Fatal("改名回滚或错误原因错误", operationError)
				}
			})
		}
	})
	// 3. 外部锁等待服从期限，其他账号的设备锁保持时也可立即拒绝归属。
	for _, foreign := range []bool{false, true} {
		label := "owned locked target honors deadline"
		if foreign {
			label = "foreign locked target returns not found"
		}
		t.Run(label, func(t *testing.T) {
			caller := newActor(t, 1, uuid.NewString())
			userID := uint64(1)
			if foreign {
				userID = 3
			}
			target := newActor(t, userID, uuid.NewString())
			baseline := read(t, target.DeviceID)
			holding, err := pool.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer holding.Rollback()
			var lockedID uint64
			if err := holding.QueryRowContext(ctx, "SELECT id FROM user_devices WHERE id=? FOR UPDATE", target.DeviceID).Scan(&lockedID); err != nil {
				t.Fatal(err)
			}
			requestContext, stop := context.WithTimeout(ctx, 200*time.Millisecond)
			defer stop()
			err = store.WithUserTransaction(requestContext, caller, func(txContext context.Context, txStore *Store) error {
				_, err := txStore.RenameUserDevice(txContext, target.DeviceID, "等待名称")
				return err
			})
			want := error(context.DeadlineExceeded)
			if foreign {
				want = domain.ErrDeviceNotFound
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(read(t, target.DeviceID), baseline) {
				t.Fatal("行锁等待、归属或回滚错误", err)
			}
		})
	}
	t.Run("concurrent names serialize through user lock", func(t *testing.T) {
		caller := newActor(t, 1, uuid.NewString())
		otherCaller := newActor(t, 1, uuid.NewString())
		target := newActor(t, 1, uuid.NewString())
		firstUpdated, releaseFirst := make(chan struct{}), make(chan struct{})
		firstResult, secondResult := make(chan error, 1), make(chan error, 1)
		var once sync.Once
		release := func() { once.Do(func() { close(releaseFirst) }) }
		defer release()
		go func() {
			firstResult <- store.WithUserTransaction(ctx, caller, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.RenameUserDevice(txContext, target.DeviceID, "第一次名称"); err != nil {
					return err
				}
				close(firstUpdated)
				<-releaseFirst
				return nil
			})
		}()
		select {
		case <-firstUpdated:
		case <-time.After(2 * time.Second):
			t.Fatal("第一次改名未进入事务等待点")
		}
		go func() {
			_, err := rename(otherCaller, target.DeviceID, "第二次名称")
			secondResult <- err
		}()
		select {
		case err := <-secondResult:
			t.Fatal("前一事务提交前并发改名已结束", err)
		case <-time.After(80 * time.Millisecond):
		}
		release()
		for _, result := range []chan error{firstResult, secondResult} {
			select {
			case err := <-result:
				if err != nil {
					t.Fatal("并发改名失败", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("并发改名未结束")
			}
		}
		if read(t, target.DeviceID).name != "第二次名称" {
			t.Fatal("后一次事务未读取并更新设备当前名称")
		}
	})
	t.Run("caller status rechecked before callback", func(t *testing.T) {
		for _, mode := range []string{"disabled account", "revoked session", "revoked device", "expired session"} {
			t.Run(mode, func(t *testing.T) {
				caller, target := newActor(t, 1, uuid.NewString()), newActor(t, 1, uuid.NewString())
				baseline := read(t, target.DeviceID)
				var statement string
				var arguments []any
				var want error
				switch mode {
				case "disabled account":
					statement, want = "UPDATE users SET status='DISABLED' WHERE id=1", domain.ErrAccountDisabled
					defer func() {
						if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
							t.Error("恢复独占库账号失败", err)
						}
					}()
				case "revoked session":
					statement, arguments, want = "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", []any{caller.SessionID}, domain.ErrSessionRevoked
				case "revoked device":
					statement, arguments, want = "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", []any{caller.DeviceID}, domain.ErrDeviceRevoked
				default:
					statement, arguments, want = "UPDATE auth_sessions SET expires_at=DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 1 MINUTE) WHERE id=?", []any{caller.SessionID}, domain.ErrSessionExpired
				}
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal(err)
				}
				called := false
				err := store.WithUserTransaction(ctx, caller, func(txContext context.Context, txStore *Store) error {
					called = true
					_, err := txStore.RenameUserDevice(txContext, target.DeviceID, "状态复查名称")
					return err
				})
				if called || !errors.Is(err, want) || !reflect.DeepEqual(read(t, target.DeviceID), baseline) {
					t.Fatal("失效身份进入改名回调或修改了目标", err)
				}
			})
		}
	})
	t.Run("full unsigned device id", func(t *testing.T) {
		caller := newActor(t, 1, uuid.NewString())
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,1,?,'原设备','ANDROID')", maximum, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		result, err := rename(caller, maximum, "完整 ID 设备")
		if err != nil || result == nil || result.ID != maximum || result.Name != "完整 ID 设备" || read(t, maximum).name != result.Name {
			t.Fatal("完整 uint64 目标改名失败", err)
		}
	})
	if err := pool.PingContext(ctx); err != nil {
		t.Fatal("改名操作关闭了共享连接池", err)
	}
}
