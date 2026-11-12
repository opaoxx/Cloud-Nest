package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestWithUserTransactionIntegration(t *testing.T) {
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
	newActor := func(t *testing.T, userID uint64) domain.Actor {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "用户事务测试设备", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		token, err := signer.Issue(*session)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("真实 Actor 授权失败", err)
		}
		return *actor
	}
	readProbe := func(t *testing.T) int {
		t.Helper()
		var value int
		if err := pool.QueryRowContext(ctx, "SELECT probe_value FROM users WHERE id=1").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	writeProbe := func(txContext context.Context, txStore *Store) error {
		return txStore.database.WithContext(txContext).Exec("UPDATE users SET probe_value=probe_value+1 WHERE id=1").Error
	}
	t.Run("single connection and read only state", func(t *testing.T) {
		actor := newActor(t, 1)
		var before, after time.Time
		if err := pool.QueryRowContext(ctx, "SELECT updated_at FROM auth_sessions WHERE id=?", actor.SessionID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(10)
		called := false
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			called = true
			current, err := txStore.FindSessionAuthorization(txContext, actor.UserID, actor.SessionID)
			if err != nil {
				return err
			}
			if current.DeviceID != actor.DeviceID {
				return fmt.Errorf("事务内查询归属错误")
			}
			return nil
		})
		if err != nil || !called {
			t.Fatal("回调未复用事务连接", err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT updated_at FROM auth_sessions WHERE id=?", actor.SessionID).Scan(&after); err != nil || !after.Equal(before) {
			t.Fatal("复查改变了更新时间", err)
		}
	})
	t.Run("commit error panic and cancellation", func(t *testing.T) {
		actor := newActor(t, 1)
		before := readProbe(t)
		if err := store.WithUserTransaction(ctx, actor, writeProbe); err != nil || readProbe(t) != before+1 {
			t.Fatal("提交失败", err)
		}
		failure := errors.New("fixture callback failure")
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if err := writeProbe(txContext, txStore); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) || readProbe(t) != before+1 {
			t.Fatal("错误回调未回滚", err)
		}
		func() {
			defer func() {
				if recover() != failure {
					t.Error("panic 未按原值传播")
				}
			}()
			_ = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				if err := writeProbe(txContext, txStore); err != nil {
					t.Error(err)
					return err
				}
				panic(failure)
			})
		}()
		requestContext, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		err = store.WithUserTransaction(requestContext, actor, func(txContext context.Context, txStore *Store) error {
			if err := writeProbe(txContext, txStore); err != nil {
				return err
			}
			cancelRequest()
			return nil
		})
		if !errors.Is(err, context.Canceled) || readProbe(t) != before+1 {
			t.Fatal("取消或 panic 后的写入未回滚", err)
		}
	})
	for _, testCase := range []struct {
		name, statement string
		want            error
	}{
		{name: "disabled after authorization", statement: "UPDATE users SET status='DISABLED' WHERE id=?", want: domain.ErrAccountDisabled},
		{name: "session revoked after authorization", statement: "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", want: domain.ErrSessionRevoked},
		{name: "device revoked after authorization", statement: "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", want: domain.ErrDeviceRevoked},
		{name: "session expired after authorization", statement: "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=?", want: domain.ErrSessionExpired},
		{name: "session removed after authorization", statement: "DELETE FROM auth_sessions WHERE id=?", want: domain.ErrSessionNotFound},
		{name: "device removed after authorization", statement: "DELETE FROM user_devices WHERE id=?", want: domain.ErrSessionNotFound},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor := newActor(t, 1)
			id := actor.SessionID
			if testCase.want == domain.ErrAccountDisabled {
				id = actor.UserID
				t.Cleanup(func() {
					if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
						t.Error(err)
					}
				})
			}
			if testCase.want == domain.ErrDeviceRevoked || testCase.name == "device removed after authorization" {
				id = actor.DeviceID
			}
			if testCase.name == "device removed after authorization" {
				if _, err := pool.ExecContext(ctx, "DELETE FROM auth_sessions WHERE id=?", actor.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.ExecContext(ctx, testCase.statement, id); err != nil {
				t.Fatal(err)
			}
			err := store.WithUserTransaction(ctx, actor, func(context.Context, *Store) error { t.Error("失效 Actor 进入了写回调"); return nil })
			if !errors.Is(err, testCase.want) {
				t.Fatal("锁内状态复查错误", err)
			}
		})
	}
	t.Run("another users device and session", func(t *testing.T) {
		actor, another := newActor(t, 1), newActor(t, 3)
		for _, field := range []string{"device", "session"} {
			candidate := actor
			if field == "device" {
				candidate.DeviceID = another.DeviceID
			} else {
				candidate.SessionID = another.SessionID
			}
			err := store.WithUserTransaction(ctx, candidate, func(context.Context, *Store) error { t.Error("跨账号标识进入回调"); return nil })
			if !errors.Is(err, domain.ErrSessionNotFound) {
				t.Fatal("跨账号标识未按归属隔离", err)
			}
		}
	})
	t.Run("all locks and latest revocation after waiting", func(t *testing.T) {
		actor, another := newActor(t, 1), newActor(t, 3)
		entered, release := make(chan struct{}), make(chan struct{})
		firstDone := make(chan error, 1)
		var releaseOnce sync.Once
		releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseFirst()
		go func() {
			firstDone <- store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				close(entered)
				select {
				case <-release:
				case <-txContext.Done():
					return txContext.Err()
				}
				// 测试夹具模拟会话撤销，观察等待者能否读到提交后的状态。
				return txStore.database.WithContext(txContext).Exec("UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.SessionID).Error
			})
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("首个事务未取得锁")
		}
		for _, target := range []struct {
			table string
			id    uint64
		}{
			{table: "users", id: actor.UserID}, {table: "user_devices", id: actor.DeviceID}, {table: "auth_sessions", id: actor.SessionID},
		} {
			lockContext, cancelLock := context.WithTimeout(ctx, 80*time.Millisecond)
			_, err := pool.ExecContext(lockContext, "UPDATE "+target.table+" SET id=id WHERE id=?", target.id)
			cancelLock()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("目标行未锁定", target.table, err)
			}
		}
		otherContext, cancelOther := context.WithTimeout(ctx, 500*time.Millisecond)
		err := store.WithUserTransaction(otherContext, another, func(context.Context, *Store) error { return nil })
		cancelOther()
		if err != nil {
			t.Fatal("另一用户被当前用户行锁阻塞", err)
		}
		waitingDone := make(chan error, 1)
		go func() {
			waitingDone <- store.WithUserTransaction(ctx, actor, func(context.Context, *Store) error { return fmt.Errorf("等待者不应在撤销后进入回调") })
		}()
		select {
		case err := <-waitingDone:
			t.Fatal("等待者在首个事务结束前已返回", err)
		case <-time.After(80 * time.Millisecond):
		}
		releaseFirst()
		select {
		case err := <-firstDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("首个事务未结束")
		}
		select {
		case err := <-waitingDone:
			if !errors.Is(err, domain.ErrSessionRevoked) {
				t.Fatal("等待后未复查最新撤销状态", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("等待事务未结束")
		}
	})
	t.Run("maximum unsigned actor identifiers", func(t *testing.T) {
		maximum := ^uint64(0)
		_, hash := service.GenerateRefreshToken()
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,status) VALUES(?,'ACTIVE')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,?,'最大标识','WEB')", maximum, maximum, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		session := domain.LoginSession{ID: maximum, UserID: maximum, DeviceID: maximum, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,?,?)", maximum, maximum, maximum, hash[:], session.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		token, err := signer.Issue(session)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := authorization.Authorize(ctx, token.Token)
		if err != nil || actor == nil {
			t.Fatal("最大 Actor 授权失败", err)
		}
		called := false
		err = store.WithUserTransaction(ctx, *actor, func(context.Context, *Store) error { called = true; return nil })
		if err != nil || !called {
			t.Fatal("完整 uint64 Actor 事务失败", err)
		}
	})
}
