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

func TestRevokeUserSessionResults(t *testing.T) {
	queryFailure, writeFailure := errors.New("target query fault"), errors.New("target write fault")
	callbackFailure, commitFailure := errors.New("callback fault"), errors.New("commit fault")
	for _, testCase := range []struct {
		name                                                                      string
		change                                                                    func(*domain.Actor, *uint64, *[]driver.Value)
		scopeError, queryError, writeError, callbackError, commitError, wantError error
		noWrite                                                                   bool
		rows                                                                      int64
	}{
		{name: "other device", rows: 1},
		{name: "same device", rows: 1, change: func(actor *domain.Actor, _ *uint64, values *[]driver.Value) {
			(*values)[2] = strconv.FormatUint(actor.DeviceID, 10)
		}},
		{name: "current session", rows: 1, change: func(actor *domain.Actor, targetID *uint64, values *[]driver.Value) {
			*targetID = actor.SessionID
			(*values)[0], (*values)[2] = strconv.FormatUint(actor.SessionID, 10), strconv.FormatUint(actor.DeviceID, 10)
		}},
		{name: "full unsigned target", rows: 1, change: func(_ *domain.Actor, targetID *uint64, values *[]driver.Value) {
			*targetID = ^uint64(0)
			(*values)[0], (*values)[2] = "18446744073709551615", "18446744073709551615"
		}},
		{name: "expired target", rows: 1, change: func(_ *domain.Actor, _ *uint64, values *[]driver.Value) { (*values)[3] = time.Now().Add(-time.Hour) }},
		{name: "already revoked", noWrite: true, change: func(_ *domain.Actor, _ *uint64, values *[]driver.Value) { (*values)[4] = time.Now().Add(-time.Minute) }},
		{name: "missing or foreign", noWrite: true, scopeError: domain.ErrSessionNotFound, wantError: domain.ErrSessionNotFound},
		{name: "scope query failure", noWrite: true, scopeError: queryFailure, wantError: queryFailure},
		{name: "target removed after scope read", noWrite: true, wantError: domain.ErrSessionNotFound, change: func(_ *domain.Actor, _ *uint64, values *[]driver.Value) { *values = nil }},
		{name: "query failure", noWrite: true, queryError: queryFailure, wantError: queryFailure},
		{name: "wrong target", noWrite: true, wantError: domain.ErrInvalidSessionData, change: func(_ *domain.Actor, _ *uint64, values *[]driver.Value) { (*values)[0] = "99" }},
		{name: "wrong owner", noWrite: true, wantError: domain.ErrInvalidSessionData, change: func(_ *domain.Actor, _ *uint64, values *[]driver.Value) { (*values)[1] = "99" }},
		{name: "zero device", noWrite: true, wantError: domain.ErrInvalidSessionData, change: func(_ *domain.Actor, _ *uint64, values *[]driver.Value) { (*values)[2] = "0" }},
		{name: "missing expiration", noWrite: true, wantError: domain.ErrInvalidSessionData, change: func(_ *domain.Actor, _ *uint64, values *[]driver.Value) { (*values)[3] = time.Time{} }},
		{name: "no updated row", wantError: domain.ErrInvalidSessionData},
		{name: "multiple updated rows", rows: 2, wantError: domain.ErrInvalidSessionData},
		{name: "negative updated rows", rows: -1, wantError: domain.ErrInvalidSessionData},
		{name: "write failure", writeError: writeFailure, wantError: writeFailure},
		{name: "callback failure", rows: 1, callbackError: callbackFailure, wantError: callbackFailure},
		{name: "commit failure", rows: 1, commitError: commitFailure, wantError: commitFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 复用真实 database/sql 事务脚本，分别模拟当前身份及目标会话。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			targetID := uint64(19)
			values := []driver.Value{"19", "7", "23", time.Now().Add(time.Hour), nil}
			if testCase.change != nil {
				testCase.change(&actor, &targetID, &values)
			}
			script := &loginTransactionScript{commitError: testCase.commitError}
			store := revokeSessionTestStore(t, actor, script)
			currentQuery := script.sessionQuery
			scopeQueries, targetQueries, writes := 0, 0, 0
			var transactionContext context.Context
			script.sessionQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(statement, "device_id = ?") {
					return currentQuery(ctx, statement, arguments)
				}
				if strings.Contains(statement, "JOIN users AS accounts") {
					scopeQueries++
					if strings.Contains(statement, "FOR UPDATE") || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != targetID {
						t.Error("归属定位未使用普通用户范围查询")
					}
					if errors.Is(testCase.scopeError, domain.ErrSessionNotFound) {
						return &sessionAuthorizationRows{}, nil
					}
					return &sessionAuthorizationRows{values: []driver.Value{strconv.FormatUint(actor.UserID, 10), strconv.FormatUint(targetID, 10), "23", "ACTIVE", time.Now().Add(time.Hour), nil, nil}}, testCase.scopeError
				}
				targetQueries++
				if strings.ReplaceAll(statement, "\x60", "") != "SELECT id,user_id,device_id,expires_at,revoked_at FROM auth_sessions WHERE user_id = ? AND id = ? LIMIT ? FOR UPDATE" ||
					len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != targetID || arguments[2].Value != int64(1) {
					t.Error("目标查询缺少归属、主键、显式字段或当前读锁")
				}
				parentDeadline, _ := transactionContext.Deadline()
				queryDeadline, exists := ctx.Deadline()
				if !exists || !queryDeadline.Equal(parentDeadline) {
					t.Error("目标查询未服从外层事务期限")
				}
				return &lockedSessionRows{values: values}, testCase.queryError
			}
			script.revoke = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				targetDeviceID, err := strconv.ParseUint(values[2].(string), 10, 64)
				if err != nil {
					t.Fatal("测试设备 ID 无效")
				}
				if statement != "UPDATE auth_sessions SET revoked_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND device_id = ? AND id = ? AND revoked_at IS NULL" ||
					len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != targetDeviceID || arguments[2].Value != targetID {
					t.Error("更新未使用锁内目标设备、归属与会话，或覆盖了其他字段")
				}
				return driver.RowsAffected(testCase.rows), testCase.writeError
			}

			// 2. 调用方法，把故障交给外层事务处理。
			err := store.WithUserTransaction(context.Background(), actor, func(ctx context.Context, txStore *Store) error {
				transactionContext = ctx
				if err := txStore.RevokeUserSession(ctx, targetID); err != nil {
					return err
				}
				return testCase.callbackError
			})
			if (testCase.wantError == nil && err != nil) || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
				t.Fatal("撤销结果或错误原因错误", err)
			}

			// 3. 核对目标查询、写入次数及提交/回滚顺序。
			expectedWrites := 1
			if testCase.noWrite {
				expectedWrites = 0
			}
			ending := ",rollback"
			if testCase.wantError == nil || testCase.commitError != nil {
				ending = ",commit"
			}
			expectedTargetQueries := 1
			if testCase.scopeError != nil {
				expectedTargetQueries = 0
			}
			if scopeQueries != 1 || targetQueries != expectedTargetQueries || writes != expectedWrites || !strings.HasPrefix(script.sequence(), "begin,lock,device,session,session") || !strings.HasSuffix(script.sequence(), ending) {
				t.Fatal("目标操作次数或事务生命周期错误", script.sequence())
			}
		})
	}
}

func TestRevokeUserSessionBoundary(t *testing.T) {
	for _, name := range []string{"ordinary store", "login transaction", "bound store without transaction", "nil context", "zero target", "wrong owner", "zero actor device", "zero actor session", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				targetID := uint64(19)
				switch name {
				case "nil context":
					ctx = nil
				case "zero target":
					targetID = 0
				case "wrong owner":
					txStore.loginUserID = 99
				case "zero actor device":
					txStore.authenticatedActor.DeviceID = 0
				case "zero actor session":
					txStore.authenticatedActor.SessionID = 0
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = cancelled
				}
				return txStore.RevokeUserSession(ctx, targetID)
			}
			var err error
			switch name {
			case "ordinary store":
				err = action(context.Background(), store)
			case "login transaction":
				err = store.WithLoginUserTransaction(context.Background(), actor.UserID, action)
			case "bound store without transaction":
				err = action(context.Background(), &Store{database: store.database, loginUserID: actor.UserID, authenticatedActor: &actor})
			default:
				err = store.WithUserTransaction(context.Background(), actor, action)
			}
			if err == nil || strings.Count(script.sequence(), "session") > 1 || strings.Contains(script.sequence(), "revoke") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效入口触发目标查询、写入或提交", err, script.sequence())
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因丢失")
			}
		})
	}
}

func TestRevokeUserSessionCancellation(t *testing.T) {
	for _, stage := range []string{"during scope query", "after scope query", "during target query", "after target query", "during update", "after update"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			currentQuery := script.sessionQuery
			script.sessionQuery = func(queryContext context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(statement, "device_id = ?") {
					return currentQuery(queryContext, statement, arguments)
				}
				if strings.Contains(statement, "JOIN users AS accounts") {
					if stage == "during scope query" {
						<-queryContext.Done()
						return nil, queryContext.Err()
					}
					if stage == "after scope query" {
						cancel()
					}
					return &sessionAuthorizationRows{values: []driver.Value{"7", "19", "23", "ACTIVE", time.Now().Add(time.Hour), nil, nil}}, nil
				}
				if stage == "during target query" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if stage == "after target query" {
					cancel()
				}
				return &lockedSessionRows{values: []driver.Value{"19", "7", "23", time.Now().Add(time.Hour), nil}}, nil
			}
			script.revoke = func(updateContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if stage == "during update" {
					<-updateContext.Done()
					return nil, updateContext.Err()
				}
				cancel()
				return driver.RowsAffected(1), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return txStore.RevokeUserSession(txContext, 19)
			})
			want := error(context.Canceled)
			if strings.HasPrefix(stage, "during") {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || strings.Contains(script.sequence(), "commit") || (strings.Contains(stage, "query") && strings.Contains(script.sequence(), "revoke")) {
				t.Fatal("目标操作取消后仍写入或提交，或取消原因丢失", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消后未回滚")
			}
		})
	}
}
