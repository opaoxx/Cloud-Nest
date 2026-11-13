package repository

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUserDeviceServiceIntegration(t *testing.T) {
	// 1. 使用独占库和真实 JWT 授权，向设备列表服务传入已认证身份。
	pool := loginSessionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	devices := service.NewUserDeviceService(store.ListUserDevices)
	var historical *domain.LoginDevice
	if err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
		var err error
		historical, err = txStore.CreateLoginDevice(txContext, uuid.NewString(), "历史设备", "ANDROID")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	createSession := func(t *testing.T, userID uint64, installation, name, platform string) domain.LoginSession {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
		var created *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, installation, name, platform)
			if err != nil {
				return err
			}
			created, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().UTC().Add(time.Hour))
			return err
		})
		if err != nil || created == nil {
			t.Fatal("创建设备服务会话夹具失败", err)
		}
		return *created
	}
	authorizeSession := func(t *testing.T, session domain.LoginSession) domain.Actor {
		t.Helper()
		token, err := signer.Issue(session)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("真实会话授权失败", err)
		}
		return *actor
	}
	installation := uuid.NewString()
	current := createSession(t, 1, installation, "学习浏览器 📁", "WEB")
	sameDevice := createSession(t, 1, installation, "学习浏览器 📁", "WEB")
	newer := createSession(t, 1, uuid.NewString(), "桌面设备", "WINDOWS")
	foreign := createSession(t, 3, installation, "其他账号设备", "ANDROID")
	actor, sameDeviceActor := authorizeSession(t, current), authorizeSession(t, sameDevice)
	otherActor, foreignActor := authorizeSession(t, newer), authorizeSession(t, foreign)
	if actor.DeviceID != sameDeviceActor.DeviceID || actor.SessionID == sameDeviceActor.SessionID || actor.DeviceID == actor.SessionID {
		t.Fatal("夹具未区分同设备会话与设备身份")
	}
	checkList := func(t *testing.T, identity domain.Actor, beforeID uint64, pageSize int, identifiers []uint64, hasMore bool) *domain.UserDeviceList {
		t.Helper()
		result, err := devices.List(ctx, identity, beforeID, pageSize)
		if err != nil || result == nil || result.Devices == nil || len(result.Devices) != len(identifiers) || result.HasMore != hasMore {
			t.Fatal("设备服务结果、空列表或分页错误", err)
		}
		for index, item := range result.Devices {
			if item.ID != identifiers[index] || item.UserID != identity.UserID || item.Current != (item.ID == identity.DeviceID) {
				t.Fatal("设备归属、排序或当前设备标记错误")
			}
		}
		return result
	}

	// 2. 同设备多个会话使用相同设备标记，后续页继续按请求设备定位。
	t.Run("pagination and device specific current marker", func(t *testing.T) {
		first := checkList(t, actor, 0, 2, []uint64{newer.DeviceID, current.DeviceID}, true)
		if first.Devices[0].Current || !first.Devices[1].Current {
			t.Fatal("首页当前设备标记错误")
		}
		checkList(t, actor, first.Devices[1].ID, 2, []uint64{historical.ID}, false)
		same := checkList(t, sameDeviceActor, 0, 2, []uint64{newer.DeviceID, current.DeviceID}, true)
		if same.Devices[0].Current || !same.Devices[1].Current {
			t.Fatal("同设备另一会话得到不同当前设备标记")
		}
		checkList(t, actor, newer.DeviceID, 2, []uint64{current.DeviceID, historical.ID}, false)
		other := checkList(t, otherActor, 0, 2, []uint64{newer.DeviceID, current.DeviceID}, true)
		if !other.Devices[0].Current || other.Devices[1].Current {
			t.Fatal("切换设备后当前标记错误")
		}
	})
	t.Run("owner scope empty page and device without sessions", func(t *testing.T) {
		checkList(t, foreignActor, 0, 100, []uint64{foreign.DeviceID}, false)
		checkList(t, actor, foreign.DeviceID, 2, []uint64{newer.DeviceID, current.DeviceID}, true)
		checkList(t, actor, historical.ID, 2, []uint64{}, false)
		checkList(t, actor, ^uint64(0), 100, []uint64{newer.DeviceID, current.DeviceID, historical.ID}, false)
	})
	t.Run("historical and nullable timestamps", func(t *testing.T) {
		changedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET last_seen_at=?,revoked_at=? WHERE id=?", changedAt, changedAt, historical.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET last_seen_at=? WHERE id=?", changedAt, newer.DeviceID); err != nil {
			t.Fatal(err)
		}
		result := checkList(t, actor, 0, 100, []uint64{newer.DeviceID, current.DeviceID, historical.ID}, false)
		first, middle, last := result.Devices[0], result.Devices[1], result.Devices[2]
		if first.Platform != "WINDOWS" || first.LastSeenAt == nil || !first.LastSeenAt.Equal(changedAt) || first.RevokedAt != nil ||
			middle.Name != "学习浏览器 📁" || middle.LastSeenAt != nil || middle.RevokedAt != nil ||
			last.LastSeenAt == nil || last.RevokedAt == nil || !last.LastSeenAt.Equal(changedAt) || !last.RevokedAt.Equal(changedAt) {
			t.Fatal("设备名称、平台、历史时间或 NULL 事实错误")
		}
		if fresh := authorizeSession(t, current); fresh != actor {
			t.Fatal("读取设备列表后当前会话授权异常")
		}
	})

	// 3. 完整无符号标识经过真实签名、数据库授权和服务，再检查连接故障。
	t.Run("full unsigned identity", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,status) VALUES(?,'fixture_max','fixture_hash','ACTIVE')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,?,'最大设备','ANDROID')", maximum, maximum, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		_, hash := service.GenerateRefreshToken()
		session := domain.LoginSession{ID: maximum, UserID: maximum, DeviceID: maximum, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,?,?)", maximum, maximum, maximum, hash[:], session.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		maximumActor := authorizeSession(t, session)
		checkList(t, maximumActor, 0, 1, []uint64{maximum}, false)
		checkList(t, maximumActor, maximum, 1, []uint64{}, false)
	})
	t.Run("connection failure", func(t *testing.T) {
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal(err)
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if result, err := devices.List(ctx, actor, 0, 50); result != nil || err == nil {
			t.Fatal("数据库故障被作为成功空列表返回")
		}
	})
}
