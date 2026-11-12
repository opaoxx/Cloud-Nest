package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func revokeSessionTestStore(t *testing.T, actor domain.Actor, script *loginTransactionScript) *Store {
	t.Helper()
	script.status = "ACTIVE"
	script.deviceQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
		return &lockedDeviceRows{values: []driver.Value{strconv.FormatUint(actor.DeviceID, 10), strconv.FormatUint(actor.UserID, 10), nil}}, nil
	}
	script.sessionQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
		return &lockedSessionRows{values: []driver.Value{strconv.FormatUint(actor.SessionID, 10), strconv.FormatUint(actor.UserID, 10), strconv.FormatUint(actor.DeviceID, 10), time.Now().Add(time.Hour), nil}}, nil
	}
	return loginTransactionTestStore(t, script)
}

func TestRevokeCurrentSessionResults(t *testing.T) {
	writeFailure, commitFailure, callbackFailure := errors.New("write fault"), errors.New("commit fault"), errors.New("callback fault")
	for _, testCase := range []struct {
		name                                              string
		rows                                              int64
		writeError, commitError, callbackError, wantError error
		maximum                                           bool
	}{
		{name: "revoked", rows: 1},
		{name: "maximum device and session", rows: 1, maximum: true},
		{name: "no matching active session", wantError: domain.ErrSessionRevoked},
		{name: "unexpected rows", rows: 2, wantError: domain.ErrInvalidSessionData},
		{name: "write failure", writeError: writeFailure, wantError: writeFailure},
		{name: "callback failure", rows: 1, callbackError: callbackFailure, wantError: callbackFailure},
		{name: "commit failure", rows: 1, commitError: commitFailure, wantError: commitFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			if testCase.maximum {
				actor.DeviceID, actor.SessionID = ^uint64(0), ^uint64(0)
			}
			expectedActor := actor
			var callbackContext context.Context
			script := &loginTransactionScript{commitError: testCase.commitError}
			script.revoke = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				if statement != "UPDATE auth_sessions SET revoked_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND device_id = ? AND id = ? AND revoked_at IS NULL" || len(arguments) != 3 ||
					arguments[0].Value != expectedActor.UserID || arguments[1].Value != expectedActor.DeviceID || arguments[2].Value != expectedActor.SessionID {
					t.Error("撤销字段、当前身份或条件错误")
				}
				parentDeadline, _ := callbackContext.Deadline()
				updateDeadline, exists := ctx.Deadline()
				if !exists || !updateDeadline.Equal(parentDeadline) {
					t.Error("更新未服从外层事务期限")
				}
				return driver.RowsAffected(testCase.rows), testCase.writeError
			}
			store := revokeSessionTestStore(t, actor, script)
			err := store.WithUserTransaction(context.Background(), actor, func(ctx context.Context, txStore *Store) error {
				callbackContext = ctx
				// 改动调用方的 Actor 变量，当前事务仍使用入口取得的值副本。
				actor.SessionID = 99
				if err := txStore.RevokeCurrentSession(ctx); err != nil {
					return err
				}
				return testCase.callbackError
			})
			if (testCase.wantError == nil && err != nil) || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
				t.Fatal("撤销结果或错误链不正确", err)
			}
			sequence := "begin,lock,device,session,revoke,rollback"
			if testCase.wantError == nil || testCase.commitError != nil {
				sequence = "begin,lock,device,session,revoke,commit"
			}
			if script.sequence() != sequence {
				t.Fatal("撤销事务顺序错误", script.sequence())
			}
		})
	}
}

func TestRevokeCurrentSessionBoundary(t *testing.T) {
	for _, name := range []string{"ordinary store", "login transaction", "bound store without transaction", "nil context", "wrong owner", "zero device", "zero session", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				var want error
				switch name {
				case "nil context":
					ctx = nil
				case "wrong owner":
					txStore.loginUserID = 8
					want = domain.ErrInvalidSessionData
				case "zero device":
					txStore.authenticatedActor.DeviceID = 0
					want = domain.ErrInvalidSessionData
				case "zero session":
					txStore.authenticatedActor.SessionID = 0
					want = domain.ErrInvalidSessionData
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = cancelled
					want = context.Canceled
				}
				err := txStore.RevokeCurrentSession(ctx)
				if err == nil || (want != nil && !errors.Is(err, want)) {
					t.Error("无效撤销入口未正确拒绝", err)
				}
				return err
			}
			var err error
			switch name {
			case "ordinary store":
				err = action(context.Background(), store)
			case "bound store without transaction":
				err = action(context.Background(), &Store{database: store.database, loginUserID: 7, authenticatedActor: &actor})
			case "login transaction":
				err = store.WithLoginUserTransaction(context.Background(), 7, action)
			default:
				err = store.WithUserTransaction(context.Background(), actor, action)
			}
			if err == nil || strings.Contains(script.sequence(), "revoke") || strings.Contains(script.sequence(), "insert") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效入口触发了写入或提交")
			}
		})
	}
}

func TestRevokeCurrentSessionCancellation(t *testing.T) {
	for _, stage := range []string{"during update", "after update"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			script.revoke = func(updateContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if stage == "during update" {
					<-updateContext.Done()
					return nil, updateContext.Err()
				}
				cancel()
				return driver.RowsAffected(1), nil
			}
			store := revokeSessionTestStore(t, actor, script)
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return txStore.RevokeCurrentSession(txContext) })
			want := error(context.Canceled)
			if stage == "during update" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("更新取消后仍尝试提交", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("更新取消后未回滚")
			}
		})
	}
}
