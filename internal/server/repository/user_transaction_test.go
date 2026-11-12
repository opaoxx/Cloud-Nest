package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type lockedSessionRows struct {
	values []driver.Value
	read   bool
}

func (*lockedSessionRows) Columns() []string {
	return []string{"id", "user_id", "device_id", "expires_at", "revoked_at"}
}
func (*lockedSessionRows) Close() error { return nil }
func (rows *lockedSessionRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestWithUserTransactionResults(t *testing.T) {
	queryFailure, callbackFailure := errors.New("query fault"), errors.New("callback fault")
	commitFailure, rollbackFailure := errors.New("commit fault"), errors.New("rollback fault")
	for _, testCase := range []struct {
		name, status, stage                                                             string
		change                                                                          func(*domain.Actor, *[]driver.Value)
		missingUser, missingDevice, revokedDevice                                       bool
		deviceID, deviceOwner                                                           uint64
		deviceError, sessionError, callbackError, commitError, rollbackError, wantError error
		wantCallback                                                                    bool
	}{
		{name: "active", wantCallback: true},
		{name: "maximum device and session", wantCallback: true, change: func(actor *domain.Actor, values *[]driver.Value) {
			actor.DeviceID, actor.SessionID = ^uint64(0), ^uint64(0)
			(*values)[0], (*values)[2] = "18446744073709551615", "18446744073709551615"
		}},
		{name: "missing user", missingUser: true, stage: "user", wantError: domain.ErrAccountNotFound},
		{name: "disabled user", status: "DISABLED", stage: "user", wantError: domain.ErrAccountDisabled},
		{name: "unknown user state", status: "UNKNOWN", stage: "user", wantError: domain.ErrInvalidAccountData},
		{name: "missing device", missingDevice: true, stage: "device", wantError: domain.ErrSessionNotFound},
		{name: "wrong device", deviceID: 99, stage: "device", wantError: domain.ErrInvalidDeviceData},
		{name: "wrong device owner", deviceOwner: 8, stage: "device", wantError: domain.ErrInvalidDeviceData},
		{name: "device query error", deviceError: queryFailure, stage: "device", wantError: queryFailure},
		{name: "session query error", sessionError: queryFailure, wantError: queryFailure},
		{name: "missing session", change: func(_ *domain.Actor, values *[]driver.Value) { *values = nil }, wantError: domain.ErrSessionNotFound},
		{name: "wrong session", change: func(_ *domain.Actor, values *[]driver.Value) { (*values)[0] = int64(99) }, wantError: domain.ErrInvalidSessionData},
		{name: "wrong session owner", change: func(_ *domain.Actor, values *[]driver.Value) { (*values)[1] = int64(8) }, wantError: domain.ErrInvalidSessionData},
		{name: "wrong session device", change: func(_ *domain.Actor, values *[]driver.Value) { (*values)[2] = int64(99) }, wantError: domain.ErrInvalidSessionData},
		{name: "zero session expiry", change: func(_ *domain.Actor, values *[]driver.Value) { (*values)[3] = time.Time{} }, wantError: domain.ErrInvalidSessionData},
		{name: "session revoked", change: func(_ *domain.Actor, values *[]driver.Value) { (*values)[4] = time.Now() }, wantError: domain.ErrSessionRevoked},
		{name: "device revoked", revokedDevice: true, wantError: domain.ErrDeviceRevoked},
		{name: "session expired", change: func(_ *domain.Actor, values *[]driver.Value) { (*values)[3] = time.Now().Add(-time.Second) }, wantError: domain.ErrSessionExpired},
		{name: "session revocation before device", revokedDevice: true, change: func(_ *domain.Actor, values *[]driver.Value) { (*values)[4] = time.Now() }, wantError: domain.ErrSessionRevoked},
		{name: "device revocation before expiry", revokedDevice: true, change: func(_ *domain.Actor, values *[]driver.Value) { (*values)[3] = time.Now().Add(-time.Second) }, wantError: domain.ErrDeviceRevoked},
		{name: "callback error", callbackError: callbackFailure, wantError: callbackFailure, wantCallback: true},
		{name: "commit error", commitError: commitFailure, wantError: commitFailure, wantCallback: true},
		{name: "rollback error keeps callback cause", callbackError: callbackFailure, rollbackError: rollbackFailure, wantError: callbackFailure, wantCallback: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			values := []driver.Value{int64(17), int64(7), int64(11), time.Now().UTC().Add(time.Hour), nil}
			if testCase.change != nil {
				testCase.change(&actor, &values)
			}
			status := testCase.status
			if status == "" {
				status = "ACTIVE"
			}
			script := &loginTransactionScript{status: status, missing: testCase.missingUser, commitError: testCase.commitError, rollbackError: testCase.rollbackError}
			script.deviceQuery = func(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
				if strings.ReplaceAll(query, "`", "") != "SELECT id,user_id,revoked_at FROM user_devices WHERE user_id = ? AND id = ? LIMIT ? FOR UPDATE" || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != actor.DeviceID || arguments[2].Value != int64(1) {
					t.Error("设备锁字段或参数错误")
				}
				if testCase.missingDevice {
					return &lockedDeviceRows{}, nil
				}
				deviceID, owner := actor.DeviceID, actor.UserID
				if testCase.deviceID != 0 {
					deviceID = testCase.deviceID
				}
				if testCase.deviceOwner != 0 {
					owner = testCase.deviceOwner
				}
				var revoked driver.Value
				if testCase.revokedDevice {
					revoked = time.Now()
				}
				return &lockedDeviceRows{values: []driver.Value{strconv.FormatUint(deviceID, 10), strconv.FormatUint(owner, 10), revoked}}, testCase.deviceError
			}
			var lockedContext context.Context
			script.sessionQuery = func(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
				lockedContext = ctx
				if strings.ReplaceAll(query, "`", "") != "SELECT id,user_id,device_id,expires_at,revoked_at FROM auth_sessions WHERE user_id = ? AND device_id = ? AND id = ? LIMIT ? FOR UPDATE" || len(arguments) != 4 || arguments[0].Value != actor.UserID || arguments[1].Value != actor.DeviceID || arguments[2].Value != actor.SessionID || arguments[3].Value != int64(1) {
					t.Error("会话五列、归属或锁条件错误")
				}
				return &lockedSessionRows{values: values}, testCase.sessionError
			}
			store := loginTransactionTestStore(t, script)
			called := false
			err := store.WithUserTransaction(context.Background(), actor, func(ctx context.Context, txStore *Store) error {
				called = true
				script.record("callback")
				if ctx != lockedContext || txStore == store {
					t.Error("回调未收到事务上下文和 Store")
				}
				if _, valid := txStore.database.Statement.ConnPool.(*sql.Tx); !valid {
					t.Error("回调未绑定同一 sql.Tx")
				}
				return testCase.callbackError
			})
			if (testCase.wantError == nil && err != nil) || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) || called != testCase.wantCallback {
				t.Fatal("回调或错误链不符合预期", err)
			}
			if testCase.rollbackError != nil && !errors.Is(err, testCase.rollbackError) {
				t.Error("丢失回滚原因")
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
			if testCase.wantError == nil || testCase.commitError != nil {
				sequence += ",commit"
			} else {
				sequence += ",rollback"
			}
			if script.sequence() != sequence {
				t.Fatal("锁定与事务执行顺序错误", script.sequence())
			}
		})
	}
}

func TestWithUserTransactionBoundary(t *testing.T) {
	for _, name := range []string{"nil context", "zero user", "zero device", "zero session", "nil callback", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			action := func(context.Context, *Store) error { t.Error("无效输入执行回调"); return nil }
			switch name {
			case "nil context":
				callContext = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "nil callback":
				action = nil
			case "cancelled":
				cancel()
			}
			script := &loginTransactionScript{status: "ACTIVE"}
			store := loginTransactionTestStore(t, script)
			err := store.WithUserTransaction(callContext, actor, action)
			if err == nil || script.sequence() != "" {
				t.Fatal("无效入口开启了事务")
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因丢失")
			}
		})
	}
	t.Run("nested rejected", func(t *testing.T) {
		script := &loginTransactionScript{status: "ACTIVE"}
		store := loginTransactionTestStore(t, script)
		err := store.WithLoginUserTransaction(context.Background(), 7, func(ctx context.Context, txStore *Store) error {
			return txStore.WithUserTransaction(ctx, domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, func(context.Context, *Store) error { t.Error("嵌套回调被执行"); return nil })
		})
		if err == nil || script.sequence() != "begin,lock,rollback" {
			t.Fatal("嵌套未拒绝")
		}
	})
}

func TestWithUserTransactionCancellation(t *testing.T) {
	for _, stage := range []string{"device", "session", "deadline", "callback"} {
		t.Run(stage, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			script := &loginTransactionScript{status: "ACTIVE"}
			script.deviceQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				if stage == "device" {
					cancel()
				}
				return &lockedDeviceRows{values: []driver.Value{int64(11), int64(7), nil}}, nil
			}
			script.sessionQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if stage == "session" {
					cancel()
				}
				if stage == "deadline" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				return &lockedSessionRows{values: []driver.Value{int64(17), int64(7), int64(11), time.Now().Add(time.Hour), nil}}, nil
			}
			store := loginTransactionTestStore(t, script)
			called := false
			err := store.WithUserTransaction(ctx, actor, func(context.Context, *Store) error { called = true; cancel(); return nil })
			want := error(context.Canceled)
			if stage == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || called != (stage == "callback") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("取消或期限未正确结束事务", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消后没有回滚")
			}
		})
	}
}
