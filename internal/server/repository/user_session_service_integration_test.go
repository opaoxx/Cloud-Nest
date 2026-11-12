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

func TestUserSessionServiceIntegration(t *testing.T) {
	// 1. 使用独占库和真实 JWT 授权，向列表服务传入已认证身份。
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
	sessions := service.NewUserSessionService(store.ListUserSessions)
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
			t.Fatal("创建会话列表服务夹具失败", err)
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
	older := createSession(t, 1, installation, "浏览器设备", "WEB")
	current := createSession(t, 1, installation, "浏览器设备", "WEB")
	newer := createSession(t, 1, uuid.NewString(), "桌面设备", "WINDOWS")
	foreign := createSession(t, 3, uuid.NewString(), "其他账号设备", "ANDROID")
	actor := authorizeSession(t, current)
	otherActor := authorizeSession(t, newer)
	foreignActor := authorizeSession(t, foreign)
	checkList := func(t *testing.T, identity domain.Actor, beforeID uint64, pageSize int, identifiers []uint64, hasMore bool) *domain.UserSessionList {
		t.Helper()
		result, err := sessions.List(ctx, identity, beforeID, pageSize)
		if err != nil || result == nil || result.Sessions == nil || len(result.Sessions) != len(identifiers) || result.HasMore != hasMore {
			t.Fatal("真实列表服务结果或分页错误", err)
		}
		for index, item := range result.Sessions {
			if item.ID != identifiers[index] || item.UserID != identity.UserID || item.Current != (item.ID == identity.SessionID) {
				t.Fatal("归属、排序或当前会话标记错误")
			}
		}
		return result
	}
	// 2. 同设备多个会话、不同请求身份与后续页均按会话 ID 标记。
	t.Run("pagination and actor specific current marker", func(t *testing.T) {
		first := checkList(t, actor, 0, 2, []uint64{newer.ID, current.ID}, true)
		last := checkList(t, actor, first.Sessions[1].ID, 2, []uint64{older.ID}, false)
		if first.Sessions[0].Current || !first.Sessions[1].Current || last.Sessions[0].Current || last.Sessions[0].DeviceID != actor.DeviceID {
			t.Fatal("同设备其他会话的标记错误")
		}
		checkList(t, actor, newer.ID, 2, []uint64{current.ID, older.ID}, false)
		changedActor := checkList(t, otherActor, 0, 2, []uint64{newer.ID, current.ID}, true)
		if !changedActor.Sessions[0].Current || changedActor.Sessions[1].Current {
			t.Fatal("切换请求身份后仍沿用上次标记")
		}
	})
	t.Run("user scope and empty page", func(t *testing.T) {
		checkList(t, foreignActor, 0, 100, []uint64{foreign.ID}, false)
		checkList(t, actor, foreign.ID, 2, []uint64{newer.ID, current.ID}, true)
		checkList(t, actor, older.ID, 2, []uint64{}, false)
	})
	// 3. 历史状态和 NULL 继续作为事实交给后续响应层。
	t.Run("historical facts and nullable timestamps", func(t *testing.T) {
		changedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET last_seen_at=?,revoked_at=?,expires_at=? WHERE id=?", changedAt, changedAt, changedAt, older.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=? WHERE id=?", changedAt, newer.DeviceID); err != nil {
			t.Fatal(err)
		}
		result := checkList(t, actor, 0, 100, []uint64{newer.ID, current.ID, older.ID}, false)
		first, middle, last := result.Sessions[0], result.Sessions[1], result.Sessions[2]
		if first.DevicePlatform != "WINDOWS" || first.DeviceRevokedAt == nil || !first.DeviceRevokedAt.Equal(changedAt) ||
			middle.DeviceName != "浏览器设备" || middle.LastSeenAt != nil || middle.RevokedAt != nil || middle.DeviceRevokedAt != nil ||
			last.LastSeenAt == nil || last.RevokedAt == nil || !last.LastSeenAt.Equal(changedAt) || !last.RevokedAt.Equal(changedAt) || !last.ExpiresAt.Equal(changedAt) {
			t.Fatal("服务修改了设备、历史时间或 NULL 事实")
		}
		if fresh := authorizeSession(t, current); fresh != actor {
			t.Fatal("读取列表后当前会话授权异常")
		}
	})
	// 4. 最大标识经过真实签名、数据库授权和列表服务，Current 仍正确。
	t.Run("full unsigned identity", func(t *testing.T) {
		identifier := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,status) VALUES(?,'fixture_max','fixture_hash','ACTIVE')", identifier); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,?,'最大标识设备','ANDROID')", identifier, identifier, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		_, hash := service.GenerateRefreshToken()
		maximum := domain.LoginSession{ID: identifier, UserID: identifier, DeviceID: identifier, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,?,?)", identifier, identifier, identifier, hash[:], maximum.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		maximumActor := authorizeSession(t, maximum)
		checkList(t, maximumActor, 0, 1, []uint64{identifier}, false)
		checkList(t, maximumActor, identifier, 1, []uint64{}, false)
	})
	// 5. 共享池生命周期由调用方管理，关闭后错误穿过服务并返回空结果。
	t.Run("connection failure", func(t *testing.T) {
		if err := pool.PingContext(ctx); err != nil {
			t.Fatal(err)
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if result, err := sessions.List(ctx, actor, 0, 50); result != nil || err == nil {
			t.Fatal("数据库故障被包装为空列表")
		}
	})
}
