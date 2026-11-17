package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// loginTransactionScript 记录真实 database/sql 事务生命周期，故障只在测试驱动中注入。
type loginTransactionScript struct {
	mutex                                              sync.Mutex
	events                                             []string
	status                                             string
	missing                                            bool
	beginError, queryError, commitError, rollbackError error
	beforeQuery                                        func(context.Context)
	deviceQueryError                                   func(context.Context) error
	deviceQuery                                        func(context.Context, string, []driver.NamedValue) (driver.Rows, error)
	sessionQuery                                       func(context.Context, string, []driver.NamedValue) (driver.Rows, error)
	entryQuery                                         func(context.Context, string, []driver.NamedValue) (driver.Rows, error)
	objectQuery                                        func(context.Context, string, []driver.NamedValue) (driver.Rows, error)
	idempotencyQuery                                   func(context.Context, string, []driver.NamedValue) (driver.Rows, error)
	preferenceQuery                                    func(context.Context, string, []driver.NamedValue) (driver.Rows, error)
	rotate                                             func(context.Context, string, []driver.NamedValue) (driver.Result, error)
	revoke                                             func(context.Context, string, []driver.NamedValue) (driver.Result, error)
	insert                                             func(context.Context, string, []driver.NamedValue) (driver.Result, error)
	renameEntry                                        func(context.Context, string, []driver.NamedValue) (driver.Result, error)
	moveEntry                                          func(context.Context, string, []driver.NamedValue) (driver.Result, error)
	setStarred                                         func(context.Context, string, []driver.NamedValue) (driver.Result, error)
	recordAccess                                       func(context.Context, string, []driver.NamedValue) (driver.Result, error)
	restore                                            func(context.Context, string, []driver.NamedValue) (driver.Result, error)
	rolledBack                                         chan struct{}
	rollbackOnce                                       sync.Once
}

func (script *loginTransactionScript) record(event string) {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	script.events = append(script.events, event)
}
func (script *loginTransactionScript) sequence() string {
	script.mutex.Lock()
	defer script.mutex.Unlock()
	return strings.Join(script.events, ",")
}

type loginTransactionConnector struct {
	deviceConnector
	script *loginTransactionScript
}

func (connector *loginTransactionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := connector.deviceConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &loginTransactionConnection{deviceConnection: connection.(*deviceConnection), script: connector.script}, nil
}

type loginTransactionConnection struct {
	*deviceConnection
	script *loginTransactionScript
}

func (connection *loginTransactionConnection) ExecContext(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(statement, "INSERT INTO user_entry_preferences (user_id, entry_id, last_opened_at)") && connection.script.recordAccess != nil {
		connection.script.record("record_access")
		return connection.script.recordAccess(ctx, statement, arguments)
	}
	if (strings.HasPrefix(statement, "INSERT INTO user_entry_preferences ") || strings.HasPrefix(statement, "UPDATE user_entry_preferences ")) && connection.script.setStarred != nil {
		connection.script.record("set_starred")
		return connection.script.setStarred(ctx, statement, arguments)
	}
	if strings.HasPrefix(statement, "UPDATE file_entries SET parent_id ") {
		connection.script.record("move_entry")
		if connection.script.moveEntry == nil {
			return nil, errors.New("测试未登记目录项移动操作")
		}
		return connection.script.moveEntry(ctx, statement, arguments)
	}
	if strings.HasPrefix(statement, "UPDATE file_entries ") {
		connection.script.record("rename_entry")
		if connection.script.renameEntry == nil {
			return nil, errors.New("测试未登记目录项改名操作")
		}
		return connection.script.renameEntry(ctx, statement, arguments)
	}
	if strings.HasPrefix(statement, "UPDATE auth_sessions SET revoked_at ") && connection.script.revoke != nil {
		connection.script.record("revoke")
		return connection.script.revoke(ctx, statement, arguments)
	}
	if strings.HasPrefix(statement, "UPDATE auth_sessions ") && connection.script.rotate != nil {
		connection.script.record("rotate")
		return connection.script.rotate(ctx, statement, arguments)
	}
	if strings.HasPrefix(statement, "UPDATE user_devices ") {
		connection.script.record("restore")
		if connection.script.restore == nil {
			return nil, errors.New("测试未登记设备恢复操作")
		}
		return connection.script.restore(ctx, statement, arguments)
	}
	connection.script.record("insert")
	if connection.script.insert == nil {
		return nil, errors.New("测试未登记插入操作")
	}
	return connection.script.insert(ctx, statement, arguments)
}

func (connection *loginTransactionConnection) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	connection.script.record("begin")
	if connection.script.beginError != nil {
		return nil, connection.script.beginError
	}
	return &loginTransactionDriver{script: connection.script}, nil
}

type loginTransactionDriver struct{ script *loginTransactionScript }

func (transaction *loginTransactionDriver) Commit() error {
	transaction.script.record("commit")
	return transaction.script.commitError
}
func (transaction *loginTransactionDriver) Rollback() error {
	transaction.script.record("rollback")
	transaction.script.rollbackOnce.Do(func() { close(transaction.script.rolledBack) })
	return transaction.script.rollbackError
}

type lockedUserRows struct {
	values []driver.Value
	read   bool
}

func (*lockedUserRows) Columns() []string { return []string{"id", "status"} }
func (*lockedUserRows) Close() error      { return nil }
func (rows *lockedUserRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func loginTransactionTestStore(t *testing.T, script *loginTransactionScript) *Store {
	t.Helper()
	script.rolledBack = make(chan struct{})
	connector := &loginTransactionConnector{script: script}
	connector.deviceConnector.accountConnector.query = func(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
		if strings.Contains(query, "user_entry_preferences") && script.preferenceQuery != nil {
			script.record("preference")
			return script.preferenceQuery(ctx, query, arguments)
		}
		if strings.Contains(query, "idempotency_records") && script.idempotencyQuery != nil {
			script.record("idempotency")
			return script.idempotencyQuery(ctx, query, arguments)
		}
		if strings.Contains(query, "file_entries") && script.entryQuery != nil {
			script.record("entry")
			return script.entryQuery(ctx, query, arguments)
		}
		if strings.Contains(query, "stored_objects") && script.objectQuery != nil {
			script.record("object")
			return script.objectQuery(ctx, query, arguments)
		}
		if strings.Contains(query, "auth_sessions") && script.sessionQuery != nil {
			script.record("session")
			return script.sessionQuery(ctx, query, arguments)
		}
		if strings.Contains(query, "user_devices") {
			script.record("device")
			if script.deviceQuery != nil {
				return script.deviceQuery(ctx, query, arguments)
			}
			if script.deviceQueryError != nil {
				if err := script.deviceQueryError(ctx); err != nil {
					return nil, err
				}
			}
			return &deviceRows{records: [][]driver.Value{{int64(11), int64(7), "28ee758a-0727-42de-ae7d-3bb33155e148", "fixture", "WEB", nil}}}, nil
		}
		script.record("lock")
		if strings.ReplaceAll(query, "`", "") != "SELECT id,status FROM users WHERE id = ? LIMIT ? FOR UPDATE" || len(arguments) != 2 || arguments[0].Value != uint64(7) || arguments[1].Value != int64(1) {
			t.Error("用户锁查询未按主键绑定或读取了多余字段")
		}
		if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
			t.Error("事务查询缺少期限")
		}
		if script.beforeQuery != nil {
			script.beforeQuery(ctx)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if script.queryError != nil {
			return nil, script.queryError
		}
		if script.missing {
			return &lockedUserRows{}, nil
		}
		return &lockedUserRows{values: []driver.Value{int64(7), script.status}}, nil
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestWithLoginUserTransactionLifecycle(t *testing.T) {
	beginFailure, queryFailure, callbackFailure := errors.New("begin failed"), errors.New("query failed"), errors.New("callback failed")
	commitFailure, rollbackFailure := errors.New("commit failed"), errors.New("rollback failed")
	for _, testCase := range []struct {
		name, status, sequence                                                       string
		missing                                                                      bool
		beginError, queryError, callbackError, commitError, rollbackError, wantError error
		wantCallback                                                                 bool
	}{
		{name: "commit", status: "ACTIVE", sequence: "begin,lock,callback,device,commit", wantCallback: true},
		{name: "missing", missing: true, sequence: "begin,lock,rollback", wantError: domain.ErrAccountNotFound},
		{name: "disabled", status: "DISABLED", sequence: "begin,lock,rollback", wantError: domain.ErrAccountDisabled},
		{name: "unknown state", status: "UNKNOWN", sequence: "begin,lock,rollback", wantError: domain.ErrInvalidAccountData},
		{name: "begin failure", beginError: beginFailure, sequence: "begin", wantError: beginFailure},
		{name: "query failure", queryError: queryFailure, sequence: "begin,lock,rollback", wantError: queryFailure},
		{name: "callback failure", status: "ACTIVE", callbackError: callbackFailure, sequence: "begin,lock,callback,device,rollback", wantCallback: true, wantError: callbackFailure},
		{name: "commit failure", status: "ACTIVE", commitError: commitFailure, sequence: "begin,lock,callback,device,commit", wantCallback: true, wantError: commitFailure},
		{name: "callback and rollback failures", status: "ACTIVE", callbackError: callbackFailure, rollbackError: rollbackFailure, sequence: "begin,lock,callback,device,rollback", wantCallback: true, wantError: callbackFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			script := &loginTransactionScript{status: testCase.status, missing: testCase.missing, beginError: testCase.beginError, queryError: testCase.queryError, commitError: testCase.commitError, rollbackError: testCase.rollbackError}
			store := loginTransactionTestStore(t, script)
			called := false
			err := store.WithLoginUserTransaction(context.Background(), 7, func(txContext context.Context, transactionStore *Store) error {
				called = true
				script.record("callback")
				if transactionStore == store {
					t.Error("回调收到的仍是事务外 Store")
				}
				if _, bound := transactionStore.database.Statement.ConnPool.(*sql.Tx); !bound {
					t.Error("回调没有绑定同一 sql.Tx")
				}
				device, err := transactionStore.FindLoginDeviceByInstallation(txContext, 7, "28ee758a-0727-42de-ae7d-3bb33155e148")
				if err != nil || device == nil || device.ID != 11 {
					t.Error("现有 Repository 没有在事务中复用")
				}
				return testCase.callbackError
			})
			if called != testCase.wantCallback || script.sequence() != testCase.sequence {
				t.Fatalf("生命周期错误 called=%v events=%s", called, script.sequence())
			}
			if testCase.wantError == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, testCase.wantError) {
				t.Fatalf("错误原因丢失: %v", err)
			}
			if testCase.rollbackError != nil && !errors.Is(err, rollbackFailure) {
				t.Fatal("回滚失败原因未合并")
			}
		})
	}
}

func TestWithLoginUserTransactionPreconditionsAndPanic(t *testing.T) {
	script := &loginTransactionScript{status: "ACTIVE"}
	store := loginTransactionTestStore(t, script)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, testCase := range []struct {
		name      string
		ctx       context.Context
		userID    uint64
		nilAction bool
		want      error
	}{
		{name: "nil context", userID: 7},
		{name: "zero user", ctx: context.Background()},
		{name: "nil action", ctx: context.Background(), userID: 7, nilAction: true},
		{name: "canceled", ctx: ctx, userID: 7, want: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			action := func(context.Context, *Store) error { t.Error("无效入口执行了回调"); return nil }
			if testCase.nilAction {
				action = nil
			}
			err := store.WithLoginUserTransaction(testCase.ctx, testCase.userID, action)
			if err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || script.sequence() != "" {
				t.Fatal("无效入口没有在开启事务前失败")
			}
		})
	}
	t.Run("panic rolls back and propagates", func(t *testing.T) {
		payload := &struct{ value string }{value: "private-panic"}
		func() {
			defer func() {
				if recover() != payload {
					t.Error("原始 panic 被吞掉或替换")
				}
			}()
			_ = store.WithLoginUserTransaction(context.Background(), 7, func(context.Context, *Store) error { panic(payload) })
		}()
		if script.sequence() != "begin,lock,rollback" {
			t.Fatal("panic 没有回滚")
		}
	})
	t.Run("nested entry rejected", func(t *testing.T) {
		localScript := &loginTransactionScript{status: "ACTIVE"}
		localStore := loginTransactionTestStore(t, localScript)
		err := localStore.WithLoginUserTransaction(context.Background(), 7, func(txContext context.Context, txStore *Store) error {
			return txStore.WithLoginUserTransaction(txContext, 7, func(context.Context, *Store) error { t.Error("执行了嵌套回调"); return nil })
		})
		if err == nil || localScript.sequence() != "begin,lock,rollback" {
			t.Fatal("嵌套开启了事务或保存点")
		}
	})
}

func TestWithLoginUserTransactionCancellation(t *testing.T) {
	t.Run("callback query uses transaction deadline", func(t *testing.T) {
		script := &loginTransactionScript{status: "ACTIVE", deviceQueryError: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
		store := loginTransactionTestStore(t, script)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err := store.WithLoginUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
			_, err := txStore.FindLoginDeviceByInstallation(txContext, 7, "28ee758a-0727-42de-ae7d-3bb33155e148")
			return err
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("回调查询没有沿用事务期限")
		}
		select {
		case <-script.rolledBack:
		case <-time.After(time.Second):
			t.Fatal("到期事务没有回滚")
		}
		if strings.Contains(script.sequence(), "commit") {
			t.Fatal("过期后仍尝试提交")
		}
	})
	t.Run("default five seconds", func(t *testing.T) {
		script := &loginTransactionScript{status: "ACTIVE", beforeQuery: func(ctx context.Context) { <-ctx.Done() }}
		store := loginTransactionTestStore(t, script)
		started := time.Now()
		err := store.WithLoginUserTransaction(context.Background(), 7, func(context.Context, *Store) error { t.Error("过期锁查询后执行了回调"); return nil })
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) < 4*time.Second || time.Since(started) > 8*time.Second {
			t.Fatal("默认事务期限不正确")
		}
		select {
		case <-script.rolledBack:
		case <-time.After(time.Second):
			t.Fatal("事务没有回滚")
		}
	})
}
