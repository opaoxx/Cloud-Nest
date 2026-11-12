package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type lockedRefreshRows struct {
	values []driver.Value
	read   bool
}

func (*lockedRefreshRows) Columns() []string {
	return []string{"id", "user_id", "device_id", "refresh_token_hash", "token_generation", "expires_at", "revoked_at"}
}
func (*lockedRefreshRows) Close() error { return nil }
func (rows *lockedRefreshRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func refreshTransactionFixture() (domain.VerifiedRefreshSession, []driver.Value) {
	verified := domain.VerifiedRefreshSession{
		Session:         domain.LoginSession{ID: 17, UserID: 7, DeviceID: 11, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)},
		TokenGeneration: 1, TokenHash: sha256.Sum256([]byte("fixture-refresh")),
	}
	return verified, []driver.Value{int64(17), int64(7), int64(11), verified.TokenHash[:], int64(1), verified.Session.ExpiresAt, nil}
}

func TestWithRefreshUserTransactionResults(t *testing.T) {
	queryFailure, callbackFailure := errors.New("query fault"), errors.New("callback fault")
	commitFailure, rollbackFailure := errors.New("commit fault"), errors.New("rollback fault")
	for _, testCase := range []struct {
		name                                                                string
		change                                                              func(*domain.VerifiedRefreshSession, *[]driver.Value)
		status                                                              string
		missingUser, missingDevice, revokedDevice                           bool
		deviceErr, sessionErr, callbackErr, commitErr, rollbackErr, wantErr error
		stage                                                               string
		wantCallback                                                        bool
	}{
		{name: "active", wantCallback: true},
		{name: "all zero fixed length hash", wantCallback: true, change: func(verified *domain.VerifiedRefreshSession, values *[]driver.Value) {
			verified.TokenHash = [sha256.Size]byte{}
			(*values)[3] = verified.TokenHash[:]
		}},
		{name: "current expiry replaces snapshot", wantCallback: true, change: func(verified *domain.VerifiedRefreshSession, values *[]driver.Value) {
			verified.Session.ExpiresAt = time.Now().Add(-time.Hour)
		}},
		{name: "maximum session and generation", wantCallback: true, change: func(verified *domain.VerifiedRefreshSession, values *[]driver.Value) {
			verified.Session.ID, verified.TokenGeneration = ^uint64(0), ^uint64(0)
			(*values)[0], (*values)[4] = "18446744073709551615", "18446744073709551615"
		}},
		{name: "missing user", missingUser: true, wantErr: domain.ErrAccountNotFound, stage: "user"},
		{name: "disabled", status: "DISABLED", wantErr: domain.ErrAccountDisabled, stage: "user"},
		{name: "unknown status", status: "UNKNOWN", wantErr: domain.ErrInvalidAccountData, stage: "user"},
		{name: "missing device", missingDevice: true, wantErr: domain.ErrDeviceNotFound, stage: "device"},
		{name: "device query failure", deviceErr: queryFailure, wantErr: queryFailure, stage: "device"},
		{name: "session query failure", sessionErr: queryFailure, wantErr: queryFailure},
		{name: "missing session", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { *values = nil }, wantErr: domain.ErrInvalidRefreshToken},
		{name: "wrong session", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[0] = int64(18) }, wantErr: domain.ErrInvalidSessionData},
		{name: "wrong owner", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[1] = int64(8) }, wantErr: domain.ErrInvalidSessionData},
		{name: "wrong device", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[2] = int64(12) }, wantErr: domain.ErrInvalidSessionData},
		{name: "zero generation", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[4] = int64(0) }, wantErr: domain.ErrInvalidSessionData},
		{name: "short hash", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[3] = make([]byte, 31) }, wantErr: domain.ErrInvalidSessionData},
		{name: "zero expiry", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[5] = time.Time{} }, wantErr: domain.ErrInvalidSessionData},
		{name: "changed hash", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[3] = make([]byte, 32) }, wantErr: domain.ErrInvalidRefreshToken},
		{name: "changed generation", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[4] = int64(2) }, wantErr: domain.ErrInvalidRefreshToken},
		{name: "revoked session", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[6] = time.Now() }, wantErr: domain.ErrSessionRevoked},
		{name: "revoked device", revokedDevice: true, wantErr: domain.ErrDeviceRevoked},
		{name: "expired", change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) {
			(*values)[5] = time.Now().Add(-time.Hour)
		}, wantErr: domain.ErrSessionExpired},
		{name: "session revocation before device", revokedDevice: true, change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) { (*values)[6] = time.Now() }, wantErr: domain.ErrSessionRevoked},
		{name: "device revocation before expiry", revokedDevice: true, change: func(_ *domain.VerifiedRefreshSession, values *[]driver.Value) {
			(*values)[5] = time.Now().Add(-time.Hour)
		}, wantErr: domain.ErrDeviceRevoked},
		{name: "callback failure", callbackErr: callbackFailure, wantErr: callbackFailure, wantCallback: true},
		{name: "commit failure", commitErr: commitFailure, wantErr: commitFailure, wantCallback: true},
		{name: "rollback failure preserves both", callbackErr: callbackFailure, rollbackErr: rollbackFailure, wantErr: callbackFailure, wantCallback: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			verified, values := refreshTransactionFixture()
			if testCase.change != nil {
				testCase.change(&verified, &values)
			}
			status := testCase.status
			if status == "" {
				status = "ACTIVE"
			}
			script := &loginTransactionScript{status: status, missing: testCase.missingUser, commitError: testCase.commitErr, rollbackError: testCase.rollbackErr}
			script.deviceQuery = func(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
				if strings.ReplaceAll(query, "`", "") != "SELECT id,user_id,revoked_at FROM user_devices WHERE user_id = ? AND id = ? LIMIT ? FOR UPDATE" || len(args) != 3 || args[0].Value != uint64(7) || args[1].Value != uint64(11) || args[2].Value != int64(1) {
					t.Error("设备锁字段或归属参数错误")
				}
				if testCase.missingDevice {
					return &lockedDeviceRows{}, nil
				}
				var revoked driver.Value
				if testCase.revokedDevice {
					revoked = time.Now()
				}
				return &lockedDeviceRows{values: []driver.Value{int64(11), int64(7), revoked}}, testCase.deviceErr
			}
			var lockedContext context.Context
			script.sessionQuery = func(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
				lockedContext = ctx
				if strings.ReplaceAll(query, "`", "") != "SELECT id,user_id,device_id,refresh_token_hash,token_generation,expires_at,revoked_at FROM auth_sessions WHERE user_id = ? AND device_id = ? AND id = ? LIMIT ? FOR UPDATE" || len(args) != 4 || args[0].Value != uint64(7) || args[1].Value != uint64(11) || args[2].Value != verified.Session.ID || args[3].Value != int64(1) {
					t.Error("会话锁字段或归属参数错误")
				}
				return &lockedRefreshRows{values: values}, testCase.sessionErr
			}
			store := loginTransactionTestStore(t, script)
			called := false
			err := store.WithRefreshUserTransaction(context.Background(), verified, func(ctx context.Context, txStore *Store, current domain.VerifiedRefreshSession) error {
				called = true
				script.record("callback")
				if ctx != lockedContext || txStore == store {
					t.Error("回调上下文或 Store 未沿用事务")
				}
				if _, valid := txStore.database.Statement.ConnPool.(*sql.Tx); !valid {
					t.Error("回调未绑定 sql.Tx")
				}
				if current.Session.ID != verified.Session.ID || current.Session.UserID != 7 || current.Session.DeviceID != 11 || !current.Session.ExpiresAt.Equal(values[5].(time.Time)) || current.Session.ExpiresAt.Location() != time.UTC || current.TokenHash != verified.TokenHash || current.TokenGeneration != verified.TokenGeneration {
					t.Error("锁内结果未保留当前资料")
				}
				return testCase.callbackErr
			})
			if (testCase.wantErr == nil && err != nil) || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || called != testCase.wantCallback {
				t.Fatalf("错误或回调结果异常 %v", err)
			}
			if testCase.rollbackErr != nil && !errors.Is(err, testCase.rollbackErr) {
				t.Error("丢失回滚故障")
			}
			sequence := "begin,lock"
			if testCase.stage != "user" {
				sequence += ",device"
			}
			if testCase.stage != "user" && testCase.stage != "device" {
				sequence += ",session"
			}
			if called {
				sequence += ",callback"
			}
			if testCase.wantErr == nil || testCase.commitErr != nil {
				sequence += ",commit"
			} else {
				sequence += ",rollback"
			}
			if script.sequence() != sequence {
				t.Fatalf("事务顺序错误 %s", script.sequence())
			}
		})
	}
}

func TestWithRefreshUserTransactionBoundary(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*domain.VerifiedRefreshSession)
	}{
		{name: "zero user", change: func(value *domain.VerifiedRefreshSession) { value.Session.UserID = 0 }},
		{name: "zero session", change: func(value *domain.VerifiedRefreshSession) { value.Session.ID = 0 }},
		{name: "zero device", change: func(value *domain.VerifiedRefreshSession) { value.Session.DeviceID = 0 }},
		{name: "zero generation", change: func(value *domain.VerifiedRefreshSession) { value.TokenGeneration = 0 }},
		{name: "nil context"}, {name: "nil callback"}, {name: "cancelled"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			verified, _ := refreshTransactionFixture()
			if testCase.change != nil {
				testCase.change(&verified)
			}
			script := &loginTransactionScript{status: "ACTIVE"}
			store := loginTransactionTestStore(t, script)
			var ctx context.Context = context.Background()
			action := func(context.Context, *Store, domain.VerifiedRefreshSession) error {
				t.Error("无效入口执行了回调")
				return nil
			}
			if testCase.name == "nil context" {
				ctx = nil
			}
			if testCase.name == "nil callback" {
				action = nil
			}
			if testCase.name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := store.WithRefreshUserTransaction(ctx, verified, action)
			if err == nil || script.sequence() != "" {
				t.Fatal("无效输入未在开启事务前结束")
			}
			if testCase.name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因丢失")
			}
		})
	}
	t.Run("nested rejected", func(t *testing.T) {
		verified, _ := refreshTransactionFixture()
		script := &loginTransactionScript{status: "ACTIVE"}
		store := loginTransactionTestStore(t, script)
		err := store.WithLoginUserTransaction(context.Background(), 7, func(ctx context.Context, txStore *Store) error {
			return txStore.WithRefreshUserTransaction(ctx, verified, func(context.Context, *Store, domain.VerifiedRefreshSession) error {
				t.Error("嵌套回调被执行")
				return nil
			})
		})
		if err == nil || script.sequence() != "begin,lock,rollback" {
			t.Fatal("嵌套调用未被拒绝")
		}
	})
}

func TestWithRefreshUserTransactionCancellation(t *testing.T) {
	for _, stage := range []string{"device", "session", "deadline"} {
		t.Run(stage, func(t *testing.T) {
			verified, values := refreshTransactionFixture()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			script := &loginTransactionScript{status: "ACTIVE"}
			script.deviceQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				if stage == "device" {
					cancel()
				}
				return &lockedDeviceRows{values: []driver.Value{int64(11), int64(7), nil}}, nil
			}
			script.sessionQuery = func(txContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if stage == "session" {
					cancel()
				}
				if stage == "deadline" {
					<-txContext.Done()
					return nil, txContext.Err()
				}
				return &lockedRefreshRows{values: values}, nil
			}
			store := loginTransactionTestStore(t, script)
			err := store.WithRefreshUserTransaction(ctx, verified, func(context.Context, *Store, domain.VerifiedRefreshSession) error {
				t.Error("取消或到期后进入了回调")
				return nil
			})
			want := context.Canceled
			if stage == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatal("锁查询丢失上下文错误", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消后没有回滚事务")
			}
			if strings.Contains(script.sequence(), "commit") {
				t.Fatal("取消后仍尝试提交")
			}
		})
	}
}
