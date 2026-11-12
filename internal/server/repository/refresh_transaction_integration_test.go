package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestWithRefreshUserTransactionIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	verifier := service.NewRefreshCredentialService(store.FindRefreshSession)
	newFixture := func(t *testing.T) domain.VerifiedRefreshSession {
		t.Helper()
		raw, hash := service.GenerateRefreshToken()
		err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "刷新事务设备", "WEB")
			if err != nil {
				return err
			}
			_, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		verified, err := verifier.Verify(ctx, raw)
		if err != nil || verified == nil {
			t.Fatal("真实初次校验失败", err)
		}
		return *verified
	}
	probe := func(t *testing.T) int {
		t.Helper()
		var value int
		if err := pool.QueryRowContext(ctx, "SELECT probe_value FROM users WHERE id=1").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	writeProbe := func(txContext context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
		return txStore.database.WithContext(txContext).Exec("UPDATE users SET probe_value=probe_value+1 WHERE id=1").Error
	}
	t.Run("read only current result and same connection", func(t *testing.T) {
		verified := newFixture(t)
		var before, after time.Time
		if err := pool.QueryRowContext(ctx, "SELECT updated_at FROM auth_sessions WHERE id=?", verified.Session.ID).Scan(&before); err != nil {
			t.Fatal(err)
		}
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(10)
		called := false
		err := store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
			called = true
			if current != verified {
				return fmt.Errorf("锁内资料与真实初次校验不一致")
			}
			// 单连接限制下继续查询，确认回调使用同一事务连接。
			return txStore.database.WithContext(txContext).Exec("SELECT 1").Error
		})
		if err != nil || !called {
			t.Fatal("有效会话未进入同一事务回调", err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT updated_at FROM auth_sessions WHERE id=?", verified.Session.ID).Scan(&after); err != nil || !after.Equal(before) {
			t.Fatal("只读复查改变了会话更新时间", err)
		}
	})
	t.Run("commit rollback panic and cancellation", func(t *testing.T) {
		verified := newFixture(t)
		before := probe(t)
		if err := store.WithRefreshUserTransaction(ctx, verified, writeProbe); err != nil || probe(t) != before+1 {
			t.Fatal("刷新事务提交失败", err)
		}
		failure := errors.New("fixture callback failure")
		err := store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
			if err := writeProbe(txContext, txStore, current); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) || probe(t) != before+1 {
			t.Fatal("失败回调的写入未回滚", err)
		}
		func() {
			defer func() {
				if recover() != failure {
					t.Error("原 panic 未按原值传播")
				}
			}()
			_ = store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
				if err := writeProbe(txContext, txStore, current); err != nil {
					t.Error(err)
					return err
				}
				panic(failure)
			})
		}()
		cancelContext, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		err = store.WithRefreshUserTransaction(cancelContext, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
			if err := writeProbe(txContext, txStore, current); err != nil {
				return err
			}
			cancelRequest()
			return nil
		})
		if !errors.Is(err, context.Canceled) || probe(t) != before+1 {
			t.Fatal("取消或 panic 的写入未回滚", err)
		}
	})
	for _, testCase := range []struct {
		name, statement string
		want            error
	}{
		{name: "new account status", statement: "UPDATE users SET status='DISABLED' WHERE id=?", want: domain.ErrAccountDisabled},
		{name: "new session revocation", statement: "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", want: domain.ErrSessionRevoked},
		{name: "new device revocation", statement: "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", want: domain.ErrDeviceRevoked},
		{name: "new expiry", statement: "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=?", want: domain.ErrSessionExpired},
		{name: "new generation", statement: "UPDATE auth_sessions SET token_generation=2 WHERE id=?", want: domain.ErrInvalidRefreshToken},
		{name: "new digest", statement: "UPDATE auth_sessions SET refresh_token_hash=UNHEX(REPEAT('AB',32)) WHERE id=?", want: domain.ErrInvalidRefreshToken},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			verified := newFixture(t)
			id := verified.Session.ID
			if testCase.name == "new account status" {
				id = verified.Session.UserID
				t.Cleanup(func() {
					if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
						t.Error(err)
					}
				})
			}
			if testCase.name == "new device revocation" {
				id = verified.Session.DeviceID
			}
			if _, err := pool.ExecContext(ctx, testCase.statement, id); err != nil {
				t.Fatal(err)
			}
			err := store.WithRefreshUserTransaction(ctx, verified, func(context.Context, *Store, domain.VerifiedRefreshSession) error {
				t.Error("旧校验结果绕过了锁内当前状态复查")
				return nil
			})
			if !errors.Is(err, testCase.want) {
				t.Fatal("锁内复查错误", err)
			}
		})
	}
	t.Run("locks all three rows and waiting request sees committed generation", func(t *testing.T) {
		verified := newFixture(t)
		newHash := sha256.Sum256([]byte("fixture next refresh"))
		entered, release := make(chan struct{}), make(chan struct{})
		firstDone := make(chan error, 1)
		var releaseOnce sync.Once
		releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseFirst()
		go func() {
			firstDone <- store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
				close(entered)
				select {
				case <-release:
				case <-txContext.Done():
					return txContext.Err()
				}
				// 测试夹具模拟下一步的摘要替换，观察等待者能否复查到已提交值。
				return txStore.database.WithContext(txContext).Exec("UPDATE auth_sessions SET refresh_token_hash=?,token_generation=token_generation+1 WHERE id=?", newHash[:], current.Session.ID).Error
			})
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("首个事务未取得三层锁")
		}
		for _, target := range []struct {
			table string
			id    uint64
		}{
			{table: "users", id: verified.Session.UserID},
			{table: "user_devices", id: verified.Session.DeviceID},
			{table: "auth_sessions", id: verified.Session.ID},
		} {
			lockContext, cancelLock := context.WithTimeout(ctx, 80*time.Millisecond)
			_, err := pool.ExecContext(lockContext, "UPDATE "+target.table+" SET id=id WHERE id=?", target.id)
			cancelLock()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s 行未被锁定 %v", target.table, err)
			}
		}
		waitingDone := make(chan error, 1)
		go func() {
			waitingDone <- store.WithRefreshUserTransaction(ctx, verified, func(context.Context, *Store, domain.VerifiedRefreshSession) error {
				return fmt.Errorf("等待者不应在旧摘要与代次失效后进入回调")
			})
		}()
		select {
		case err := <-waitingDone:
			t.Fatalf("首个事务提交前等待者已结束 %v", err)
		case <-time.After(80 * time.Millisecond):
		}
		releaseFirst()
		select {
		case err := <-firstDone:
			if err != nil {
				t.Fatal("首个事务提交失败", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("首个事务未结束")
		}
		select {
		case err := <-waitingDone:
			if !errors.Is(err, domain.ErrInvalidRefreshToken) {
				t.Fatal("等待者未复查最新摘要和代次", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("等待者未结束")
		}
		verified.TokenHash, verified.TokenGeneration = newHash, 2
		if err := store.WithRefreshUserTransaction(ctx, verified, func(context.Context, *Store, domain.VerifiedRefreshSession) error { return nil }); err != nil {
			t.Fatal("新摘要与代次无法通过复查", err)
		}
	})
}
