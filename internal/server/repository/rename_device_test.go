package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// renameDeviceScopeRows 按归属定位查询提供 id/user_id 两列。
type renameDeviceScopeRows struct {
	values []driver.Value
	read   bool
}

func (*renameDeviceScopeRows) Columns() []string { return []string{"id", "user_id"} }
func (*renameDeviceScopeRows) Close() error      { return nil }
func (rows *renameDeviceScopeRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestRenameUserDeviceResults(t *testing.T) {
	queryFailure, writeFailure := errors.New("device query fault"), errors.New("device write fault")
	callbackFailure, commitFailure := errors.New("callback fault"), errors.New("commit fault")
	for _, testCase := range []struct {
		name, newName                                                             string
		change                                                                    func(*uint64, *[]driver.Value)
		scopeMissing                                                              bool
		scopeError, queryError, writeError, callbackError, commitError, wantError error
		noWrite                                                                   bool
		rows                                                                      int64
	}{
		{name: "other device", rows: 1},
		{name: "current device", rows: 1, change: func(targetID *uint64, values *[]driver.Value) {
			*targetID = 11
			(*values)[0] = "11"
		}},
		{name: "revoked target", rows: 1, change: func(_ *uint64, values *[]driver.Value) { (*values)[6] = time.Now().Add(-time.Hour) }},
		{name: "same name", newName: "原设备", noWrite: true},
		{name: "padded name", newName: "  Study PC 🎮  ", rows: 1},
		{name: "maximum emoji", newName: strings.Repeat("🎮", 100), rows: 1},
		{name: "case change", newName: "STUDY pc", rows: 1, change: func(_ *uint64, values *[]driver.Value) { (*values)[2] = "study PC" }},
		{name: "maximum target", rows: 1, change: func(targetID *uint64, values *[]driver.Value) {
			*targetID = ^uint64(0)
			(*values)[0] = "18446744073709551615"
		}},
		{name: "missing or foreign", scopeMissing: true, noWrite: true, wantError: domain.ErrDeviceNotFound},
		{name: "scope failure", scopeError: queryFailure, noWrite: true, wantError: queryFailure},
		{name: "target gone after scope", noWrite: true, wantError: domain.ErrDeviceNotFound, change: func(_ *uint64, values *[]driver.Value) { *values = nil }},
		{name: "target query failure", queryError: queryFailure, noWrite: true, wantError: queryFailure},
		{name: "wrong target", noWrite: true, wantError: domain.ErrInvalidDeviceData, change: func(_ *uint64, values *[]driver.Value) { (*values)[0] = "99" }},
		{name: "wrong owner", noWrite: true, wantError: domain.ErrInvalidDeviceData, change: func(_ *uint64, values *[]driver.Value) { (*values)[1] = "99" }},
		{name: "invalid creation time", noWrite: true, wantError: domain.ErrInvalidDeviceData, change: func(_ *uint64, values *[]driver.Value) { (*values)[4] = time.Time{} }},
		{name: "zero rows", wantError: domain.ErrInvalidDeviceData},
		{name: "multiple rows", rows: 2, wantError: domain.ErrInvalidDeviceData},
		{name: "negative rows", rows: -1, wantError: domain.ErrInvalidDeviceData},
		{name: "write failure", writeError: writeFailure, wantError: writeFailure},
		{name: "callback failure", rows: 1, callbackError: callbackFailure, wantError: callbackFailure},
		{name: "commit failure", rows: 1, commitError: commitFailure, wantError: commitFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 复用事务脚本，将当前设备、目标归属和目标当前读分别提供结果。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			targetID := uint64(19)
			createdAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			values := []driver.Value{"19", "7", "原设备", "WEB", createdAt, nil, nil}
			if testCase.change != nil {
				testCase.change(&targetID, &values)
			}
			name := testCase.newName
			if name == "" {
				name = "新设备名称"
			}
			script := &loginTransactionScript{commitError: testCase.commitError}
			store := revokeSessionTestStore(t, actor, script)
			currentQuery := script.deviceQuery
			scopeQueries, targetQueries, writes := 0, 0, 0
			var transactionContext context.Context
			script.deviceQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				statement = strings.ReplaceAll(statement, "`", "")
				if strings.HasPrefix(statement, "SELECT id,user_id,revoked_at ") {
					return currentQuery(ctx, statement, arguments)
				}
				if len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != targetID || arguments[2].Value != int64(1) {
					t.Error("目标查询归属或 ID 参数错误")
				}
				deadline, exists := ctx.Deadline()
				parentDeadline, _ := transactionContext.Deadline()
				if !exists || !deadline.Equal(parentDeadline) {
					t.Error("设备操作未服从事务期限")
				}
				if !strings.Contains(statement, "FOR UPDATE") {
					scopeQueries++
					if statement != "SELECT id,user_id FROM user_devices WHERE user_id = ? AND id = ? LIMIT ?" {
						t.Error("归属定位未使用只读范围查询")
					}
					if testCase.scopeMissing {
						return &renameDeviceScopeRows{}, nil
					}
					return &renameDeviceScopeRows{values: []driver.Value{strconv.FormatUint(targetID, 10), "7"}}, testCase.scopeError
				}
				targetQueries++
				if statement != "SELECT id,user_id,name,platform,created_at,last_seen_at,revoked_at FROM user_devices WHERE user_id = ? AND id = ? LIMIT ? FOR UPDATE" {
					t.Error("目标当前读字段、归属或锁错误")
				}
				var records [][]driver.Value
				if values != nil {
					records = append(records, values)
				}
				return &userDeviceRows{records: records}, testCase.queryError
			}
			script.restore = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				if statement != "UPDATE user_devices SET name = ?, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ?" || len(arguments) != 3 || arguments[0].Value != name || arguments[1].Value != actor.UserID || arguments[2].Value != targetID {
					t.Error("更新字段、原始名称或用户范围错误")
				}
				return driver.RowsAffected(testCase.rows), testCase.writeError
			}
			// 2. 执行真实 database/sql 事务生命周期，错误由外层负责回滚。
			var result *domain.UserDevice
			var operationError error
			err := store.WithUserTransaction(context.Background(), actor, func(ctx context.Context, txStore *Store) error {
				transactionContext = ctx
				result, operationError = txStore.RenameUserDevice(ctx, targetID, name)
				if operationError != nil {
					return operationError
				}
				return testCase.callbackError
			})
			if (testCase.wantError == nil && err != nil) || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
				t.Fatal("名称更新结果或错误原因错误", err)
			}
			if operationError != nil && result != nil {
				t.Fatal("操作失败返回了部分设备")
			}
			if operationError == nil && (result == nil || result.ID != targetID || result.UserID != actor.UserID || result.Name != name || result.Platform != "WEB" || !result.CreatedAt.Equal(createdAt) || (result.RevokedAt != nil) != (values[6] != nil)) {
				t.Fatal("名称更新返回记录错误")
			}
			// 3. 检查相同名称跳过写入、归属失败跳过加锁以及提交/回滚行为。
			expectedQueries, expectedWrites := 1, 1
			if testCase.scopeMissing || testCase.scopeError != nil {
				expectedQueries = 0
			}
			if testCase.noWrite {
				expectedWrites = 0
			}
			ending := ",rollback"
			if testCase.wantError == nil || testCase.commitError != nil {
				ending = ",commit"
			}
			if scopeQueries != 1 || targetQueries != expectedQueries || writes != expectedWrites || !strings.HasSuffix(script.sequence(), ending) {
				t.Fatal("设备操作次数或事务生命周期错误", script.sequence())
			}
		})
	}
}

func TestRenameUserDeviceBoundary(t *testing.T) {
	for _, name := range []string{"ordinary store", "login transaction", "bound store without transaction", "nil context", "zero target", "wrong owner", "zero actor device", "zero actor session", "cancelled", "empty name", "whitespace name", "control name", "long name", "invalid utf8 name"} {
		t.Run(name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				targetID, newName := uint64(19), "新名称"
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
				case "empty name":
					newName = ""
				case "whitespace name":
					newName = " \u3000 "
				case "control name":
					newName = "设备\n"
				case "long name":
					newName = strings.Repeat("机", 101)
				case "invalid utf8 name":
					newName = "设备\xff"
				}
				result, err := txStore.RenameUserDevice(ctx, targetID, newName)
				if result != nil || err == nil {
					t.Error("无效入口返回设备或缺少错误")
				}
				return err
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
			if err == nil || strings.Count(script.sequence(), "device") > 1 || strings.Contains(script.sequence(), "restore") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效入口触发目标查询、写入或提交", err)
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因丢失")
			}
		})
	}
}

func TestRenameUserDeviceCancellation(t *testing.T) {
	for _, stage := range []string{"during scope query", "after scope query", "during target query", "after target query", "during update", "after update"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			currentQuery := script.deviceQuery
			script.deviceQuery = func(queryContext context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if strings.HasPrefix(strings.ReplaceAll(statement, "`", ""), "SELECT id,user_id,revoked_at ") {
					return currentQuery(queryContext, statement, arguments)
				}
				scope := !strings.Contains(statement, "FOR UPDATE")
				if (scope && stage == "during scope query") || (!scope && stage == "during target query") {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if (scope && stage == "after scope query") || (!scope && stage == "after target query") {
					cancel()
				}
				if scope {
					return &renameDeviceScopeRows{values: []driver.Value{"19", "7"}}, nil
				}
				return &userDeviceRows{records: [][]driver.Value{{"19", "7", "原设备", "WEB", time.Now(), nil, nil}}}, nil
			}
			script.restore = func(updateContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if stage == "during update" {
					<-updateContext.Done()
					return nil, updateContext.Err()
				}
				cancel()
				return driver.RowsAffected(1), nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				result, err := txStore.RenameUserDevice(txContext, 19, "新名称")
				if result != nil {
					t.Error("取消后返回了设备结果")
				}
				return err
			})
			want := error(context.Canceled)
			if strings.HasPrefix(stage, "during") {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || strings.Contains(script.sequence(), "commit") || (strings.Contains(stage, "query") && strings.Contains(script.sequence(), "restore")) {
				t.Fatal("取消原因、写入或提交行为错误", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消后未回滚")
			}
		})
	}
}
