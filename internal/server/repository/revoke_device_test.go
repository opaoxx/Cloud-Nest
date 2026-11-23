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

type revokedSyncDeviceRows struct{ *idempotencyRecordRows }

func (*revokedSyncDeviceRows) Columns() []string {
	return []string{"status", "cursor_generation", "ack_sequence"}
}

func TestRevokeUserDeviceResults(t *testing.T) {
	queryFault, deviceFault, sessionFault := errors.New("query fault"), errors.New("device write fault"), errors.New("sessions write fault")
	syncFault := errors.New("sync device read fault")
	callbackFault, commitFault := errors.New("callback fault"), errors.New("commit fault")
	for _, testCase := range []struct {
		name                                                                                            string
		current, maximum, alreadyRevoked, scopeMissing, scopeInvalid, targetMissing, targetInvalid      bool
		scopeError, targetError, deviceError, sessionError, syncError, callbackError, commitError, want error
		deviceRows, sessionRows                                                                         int64
		deviceWrites, sessionWrites, targetQueries                                                      int
	}{
		{name: "other device", deviceRows: 1, sessionRows: 3, deviceWrites: 1, sessionWrites: 1, targetQueries: 1},
		{name: "current device", current: true, deviceRows: 1, sessionRows: 2, deviceWrites: 1, sessionWrites: 1, targetQueries: 1},
		{name: "maximum target", maximum: true, deviceRows: 1, sessionRows: 1, deviceWrites: 1, sessionWrites: 1, targetQueries: 1},
		{name: "no sessions", deviceRows: 1, deviceWrites: 1, sessionWrites: 1, targetQueries: 1},
		{name: "already revoked repairs sessions", alreadyRevoked: true, sessionRows: 2, sessionWrites: 1, targetQueries: 1},
		{name: "already revoked no sessions", alreadyRevoked: true, sessionWrites: 1, targetQueries: 1},
		{name: "missing or foreign", scopeMissing: true, want: domain.ErrDeviceNotFound},
		{name: "invalid scope", scopeInvalid: true, want: domain.ErrInvalidDeviceData},
		{name: "scope query fault", scopeError: queryFault, want: queryFault},
		{name: "target removed after scope", targetMissing: true, targetQueries: 1, want: domain.ErrDeviceNotFound},
		{name: "target query fault", targetError: queryFault, targetQueries: 1, want: queryFault},
		{name: "invalid target", targetInvalid: true, targetQueries: 1, want: domain.ErrInvalidDeviceData},
		{name: "zero device rows", deviceWrites: 1, targetQueries: 1, want: domain.ErrInvalidDeviceData},
		{name: "multiple device rows", deviceRows: 2, deviceWrites: 1, targetQueries: 1, want: domain.ErrInvalidDeviceData},
		{name: "negative device rows", deviceRows: -1, deviceWrites: 1, targetQueries: 1, want: domain.ErrInvalidDeviceData},
		{name: "device write fault", deviceError: deviceFault, deviceWrites: 1, targetQueries: 1, want: deviceFault},
		{name: "session write fault", deviceRows: 1, deviceWrites: 1, sessionWrites: 1, targetQueries: 1, sessionError: sessionFault, want: sessionFault},
		{name: "negative session rows", deviceRows: 1, sessionRows: -1, deviceWrites: 1, sessionWrites: 1, targetQueries: 1, want: domain.ErrInvalidSessionData},
		{name: "sync read fault", deviceRows: 1, sessionRows: 1, deviceWrites: 1, sessionWrites: 1, targetQueries: 1, syncError: syncFault, want: syncFault},
		{name: "callback fault", deviceRows: 1, sessionRows: 3, deviceWrites: 1, sessionWrites: 1, targetQueries: 1, callbackError: callbackFault, want: callbackFault},
		{name: "commit fault", deviceRows: 1, sessionRows: 3, deviceWrites: 1, sessionWrites: 1, targetQueries: 1, commitError: commitFault, want: commitFault},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 使用现有事务驱动，记录身份、目标查询、认证写入及同步锁读。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			targetID := uint64(19)
			if testCase.current {
				targetID = actor.DeviceID
			}
			if testCase.maximum {
				targetID = ^uint64(0)
			}
			script := &loginTransactionScript{commitError: testCase.commitError}
			store := revokeSessionTestStore(t, actor, script)
			currentQuery := script.deviceQuery
			deviceReads, scopeQueries, targetQueries, deviceWrites, sessionWrites := 0, 0, 0, 0, 0
			var transactionContext context.Context
			script.deviceQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				deviceReads++
				if deviceReads == 1 {
					return currentQuery(ctx, statement, arguments)
				}
				statement = strings.ReplaceAll(statement, "`", "")
				if len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != targetID || arguments[2].Value != int64(1) {
					t.Error("目标范围或主键参数错误")
				}
				deadline, _ := ctx.Deadline()
				parentDeadline, _ := transactionContext.Deadline()
				if !deadline.Equal(parentDeadline) {
					t.Error("目标查询未服从事务期限")
				}
				if !strings.Contains(statement, "FOR UPDATE") {
					scopeQueries++
					if statement != "SELECT id,user_id FROM user_devices WHERE user_id = ? AND id = ? LIMIT ?" {
						t.Error("目标归属定位未使用普通只读查询")
					}
					if testCase.scopeMissing {
						return &renameDeviceScopeRows{}, nil
					}
					owner := "7"
					if testCase.scopeInvalid {
						owner = "99"
					}
					return &renameDeviceScopeRows{values: []driver.Value{strconv.FormatUint(targetID, 10), owner}}, testCase.scopeError
				}
				targetQueries++
				if statement != "SELECT id,user_id,revoked_at FROM user_devices WHERE user_id = ? AND id = ? LIMIT ? FOR UPDATE" {
					t.Error("目标锁读取字段或条件错误")
				}
				if testCase.targetMissing {
					return &lockedDeviceRows{}, nil
				}
				owner := "7"
				if testCase.targetInvalid {
					owner = "99"
				}
				var revokedAt driver.Value
				if testCase.alreadyRevoked {
					revokedAt = time.Now().UTC().Add(-time.Hour)
				}
				return &lockedDeviceRows{values: []driver.Value{strconv.FormatUint(targetID, 10), owner, revokedAt}}, testCase.targetError
			}
			script.restore = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				deviceWrites++
				if statement != "UPDATE user_devices SET revoked_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND revoked_at IS NULL" || len(arguments) != 2 || arguments[0].Value != actor.UserID || arguments[1].Value != targetID {
					t.Error("设备撤销字段、归属或条件错误")
				}
				return driver.RowsAffected(testCase.deviceRows), testCase.deviceError
			}
			script.revoke = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				sessionWrites++
				if statement != "UPDATE auth_sessions SET revoked_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND device_id = ? AND revoked_at IS NULL" || len(arguments) != 2 || arguments[0].Value != actor.UserID || arguments[1].Value != targetID {
					t.Error("关联会话撤销范围或保留历史条件错误")
				}
				return driver.RowsAffected(testCase.sessionRows), testCase.sessionError
			}
			syncQueries := 0
			script.syncDeviceQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				syncQueries++
				if statement != "SELECT status, cursor_generation, ack_sequence FROM sync_devices WHERE user_id = ? AND device_id = ? FOR UPDATE" ||
					len(arguments) != 2 || arguments[0].Value != actor.UserID || arguments[1].Value != targetID {
					t.Error("同步行锁读未绑定目标设备和账号")
				}
				return &revokedSyncDeviceRows{&idempotencyRecordRows{}}, testCase.syncError
			}
			// 2. 把所有写入与故障交给同一个外层事务。
			err := store.WithUserTransaction(context.Background(), actor, func(ctx context.Context, txStore *Store) error {
				transactionContext = ctx
				if err := txStore.RevokeUserDevice(ctx, targetID); err != nil {
					return err
				}
				return testCase.callbackError
			})
			if (testCase.want == nil && err != nil) || (testCase.want != nil && !errors.Is(err, testCase.want)) {
				t.Fatal("撤销设备结果或错误链错误", err)
			}
			ending := ",rollback"
			if testCase.want == nil || testCase.commitError != nil {
				ending = ",commit"
			}
			if scopeQueries != 1 || targetQueries != testCase.targetQueries || deviceWrites != testCase.deviceWrites || sessionWrites != testCase.sessionWrites || !strings.HasSuffix(script.sequence(), ending) {
				t.Fatal("查询、两类写入或事务生命周期错误", script.sequence())
			}
			if deviceWrites == 1 && sessionWrites == 1 && !strings.Contains(script.sequence(), "restore,revoke") {
				t.Fatal("设备与会话写入顺序错误")
			}
			wantSyncQueries := testCase.sessionWrites
			if testCase.sessionError != nil || testCase.sessionRows < 0 {
				wantSyncQueries = 0
			}
			if syncQueries != wantSyncQueries || (syncQueries == 1 && !strings.Contains(script.sequence(), "revoke,sync_device")) {
				t.Fatal("同步行没有在会话撤销后复核", script.sequence())
			}
		})
	}
}

func TestRevokeUserDeviceBoundary(t *testing.T) {
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
				return txStore.RevokeUserDevice(ctx, targetID)
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
			if err == nil || strings.Count(script.sequence(), "device") > 1 || strings.Contains(script.sequence(), "restore") || strings.Contains(script.sequence(), "revoke") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效入口触发目标操作或提交", err)
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因丢失")
			}
		})
	}
}

func TestRevokeUserDeviceCancellation(t *testing.T) {
	for _, stage := range []string{"during scope", "after scope", "during target", "after target", "during device update", "after device update", "during session update", "after session update"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			currentQuery := script.deviceQuery
			reads := 0
			script.deviceQuery = func(queryContext context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if reads == 1 {
					return currentQuery(queryContext, statement, arguments)
				}
				scope := !strings.Contains(statement, "FOR UPDATE")
				if (scope && stage == "during scope") || (!scope && stage == "during target") {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if (scope && stage == "after scope") || (!scope && stage == "after target") {
					cancel()
				}
				if scope {
					return &renameDeviceScopeRows{values: []driver.Value{"19", "7"}}, nil
				}
				return &lockedDeviceRows{values: []driver.Value{"19", "7", nil}}, nil
			}
			script.restore = func(updateContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if stage == "during device update" {
					<-updateContext.Done()
					return nil, updateContext.Err()
				}
				if stage == "after device update" {
					cancel()
				}
				return driver.RowsAffected(1), nil
			}
			script.revoke = func(updateContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if stage == "during session update" {
					<-updateContext.Done()
					return nil, updateContext.Err()
				}
				cancel()
				return driver.RowsAffected(2), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return txStore.RevokeUserDevice(txContext, 19) })
			want := error(context.Canceled)
			if strings.HasPrefix(stage, "during") {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("撤销取消原因或提交行为错误", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消后未完成回滚")
			}
		})
	}
}
