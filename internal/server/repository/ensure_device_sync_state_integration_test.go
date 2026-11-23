package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestEnsureLoginDeviceSyncStateIntegration(t *testing.T) {
	// 1. 在独占库准备旧版本已撤销、仍保留同步代次的设备。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool := loginSessionDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	const original = "F100030400060708090A0B0C0D0E0FFE"
	for _, fixture := range []struct {
		name     string
		deviceID uint64
		status   domain.SyncDeviceStatus
		ack      uint64
	}{
		{"active baseline", 20, domain.SyncDeviceStatusActive, 7},
		{"pending baseline", 21, domain.SyncDeviceStatusResetRequired, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			execute := func(statement string, args ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			deviceID := fixture.deviceID
			oldSessionID := deviceID * 10
			installation := fmt.Sprintf("28ee758a-0727-42de-ae7d-%012d", deviceID)
			execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at) VALUES (?,1,?,'历史设备','WEB',?)`,
				deviceID, installation, stamp)
			execute(`INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,token_generation,expires_at,revoked_at)
				VALUES (?,1,?,UNHEX(REPEAT(?,32)),2,?,?)`, oldSessionID, deviceID, fmt.Sprintf("%02x", deviceID), stamp.Add(time.Hour), stamp)
			execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at,created_at,updated_at)
				VALUES (?,1,?,?,UNHEX(?),?,?,?)`, deviceID, fixture.status, fixture.ack, original, stamp, stamp, stamp)

			type state struct {
				deviceRevoked, oldSessionRevoked sql.NullTime
				oldSessionGeneration             uint64
				sessionCount                     int
				status                           domain.SyncDeviceStatus
				position                         uint64
				generation                       string
				lastSeen, created, updated       time.Time
			}
			read := func(queryContext context.Context, readStore *Store) state {
				t.Helper()
				var value state
				err := readStore.database.WithContext(queryContext).Raw(`SELECT device.revoked_at,old_session.revoked_at,
					old_session.token_generation,(SELECT COUNT(*) FROM auth_sessions WHERE user_id=1 AND device_id=device.id),
					states.status,states.ack_sequence,HEX(states.cursor_generation),states.last_seen_at,states.created_at,states.updated_at
					FROM user_devices AS device
					JOIN auth_sessions AS old_session ON old_session.user_id=device.user_id AND old_session.device_id=device.id AND old_session.id=?
					JOIN sync_devices AS states ON states.user_id=device.user_id AND states.device_id=device.id
					WHERE device.user_id=1 AND device.id=?`, oldSessionID, deviceID).
					Row().Scan(&value.deviceRevoked, &value.oldSessionRevoked, &value.oldSessionGeneration, &value.sessionCount,
					&value.status, &value.position, &value.generation, &value.lastSeen, &value.created, &value.updated)
				if err != nil {
					t.Fatal("读取设备与同步状态失败", err)
				}
				return value
			}
			before := read(ctx, store)
			assertRestored := func(current state) {
				t.Helper()
				if current.deviceRevoked.Valid || current.oldSessionRevoked != before.oldSessionRevoked ||
					current.oldSessionGeneration != before.oldSessionGeneration || current.sessionCount != 2 ||
					current.status != domain.SyncDeviceStatusResetRequired || current.position != 0 ||
					current.generation == before.generation || current.generation == strings.Repeat("0", 32) ||
					!current.lastSeen.Equal(before.lastSeen) || !current.created.Equal(before.created) || !current.updated.After(before.updated) {
					t.Fatalf("重新激活、旧会话或同步换代结果不符：%+v", current)
				}
			}
			ensure := func(txContext context.Context, txStore *Store) error {
				device, err := txStore.EnsureLoginDevice(txContext, installation, "登录名称", "ANDROID")
				if err != nil {
					return err
				}
				if device == nil || device.ID != deviceID || device.RevokedAt != nil || device.Name != "历史设备" || device.Platform != "WEB" {
					t.Fatal("重新激活没有复用原设备")
				}
				return nil
			}
			openSession := func(txContext context.Context, txStore *Store) error {
				if err := ensure(txContext, txStore); err != nil {
					return err
				}
				session, err := txStore.CreateLoginSession(txContext, deviceID, [32]byte{byte(deviceID), 0xfe}, time.Now().UTC().Add(time.Hour))
				if err == nil && (session == nil || session.DeviceID != deviceID || session.ID == oldSessionID) {
					t.Fatal("登录未为原设备创建新会话")
				}
				return err
			}

			// 2. 验证同步错误和后续会话步骤回滚时，设备恢复及代次均未提交。
			execute("UPDATE sync_devices SET cursor_generation=UNHEX(REPEAT('00',16)) WHERE device_id=?", deviceID)
			corrupt := read(ctx, store)
			err := store.WithLoginUserTransaction(ctx, 1, openSession)
			if !errors.Is(err, domain.ErrInvalidSyncDeviceData) || read(ctx, store) != corrupt {
				t.Fatal("同步错误时未回滚设备恢复或会话状态", err)
			}
			execute("UPDATE sync_devices SET cursor_generation=UNHEX(?) WHERE device_id=?", original, deviceID)
			rollback := errors.New("合成登录事务回滚")
			err = store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
				if err := openSession(txContext, txStore); err != nil {
					return err
				}
				assertRestored(read(txContext, txStore))
				return rollback
			})
			if !errors.Is(err, rollback) || read(ctx, store) != before {
				t.Fatal("外层错误时未回滚设备恢复、同步状态和会话创建", err)
			}

			// 3. 提交重新激活和新会话，随后普通登录复用同步状态。
			if err := store.WithLoginUserTransaction(ctx, 1, openSession); err != nil {
				t.Fatal(err)
			}
			committed := read(ctx, store)
			assertRestored(committed)
			if err := store.WithLoginUserTransaction(ctx, 1, ensure); err != nil || read(ctx, store) != committed {
				t.Fatal("普通设备复用改动了已提交的同步状态", err)
			}
		})
	}
}
