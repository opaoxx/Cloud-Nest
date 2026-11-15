package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// fileEntrySnapshotConnector 复用事务驱动，额外记录本轮的只读及隔离配置。
type fileEntrySnapshotConnector struct {
	loginTransactionConnector
	options      driver.TxOptions
	beginContext context.Context
}

func (connector *fileEntrySnapshotConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := connector.loginTransactionConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &fileEntrySnapshotConnection{loginTransactionConnection: connection.(*loginTransactionConnection), connector: connector}, nil
}

type fileEntrySnapshotConnection struct {
	*loginTransactionConnection
	connector *fileEntrySnapshotConnector
}

func (connection *fileEntrySnapshotConnection) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	connection.connector.options, connection.connector.beginContext = options, ctx
	return connection.loginTransactionConnection.BeginTx(ctx, options)
}

func fileEntrySnapshotTestStore(t *testing.T, script *loginTransactionScript) (*Store, *fileEntrySnapshotConnector) {
	t.Helper()
	script.rolledBack = make(chan struct{})
	connector := &fileEntrySnapshotConnector{loginTransactionConnector: loginTransactionConnector{script: script}}
	connector.deviceConnector.accountConnector.query = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
		script.record("query")
		if !strings.Contains(statement, "file_entries") || strings.Contains(statement, "FOR UPDATE") || len(arguments) != 3 {
			t.Error("快照读取使用了错误查询或锁定读")
		}
		if script.beforeQuery != nil {
			script.beforeQuery(ctx)
		}
		if script.queryError != nil {
			return nil, script.queryError
		}
		stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
		return &fileEntryRows{values: []driver.Value{int64(19), int64(7), nil, "", "DIR", int64(0), int64(1), "ACTIVE", stamp, stamp, stamp}}, nil
	}
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	return store, connector
}

func TestWithFileEntrySnapshotLifecycle(t *testing.T) {
	beginFailure, queryFailure := errors.New("fixture begin"), errors.New("fixture query")
	callbackFailure, commitFailure, rollbackFailure := errors.New("fixture callback"), errors.New("fixture commit"), errors.New("fixture rollback")
	for _, testCase := range []struct {
		name, sequence                                                    string
		reads                                                             int
		beginError, queryError, callbackError, commitError, rollbackError error
		wantError                                                         error
	}{
		{name: "empty callback", sequence: "begin,callback,commit"},
		{name: "two reads", reads: 2, sequence: "begin,callback,query,query,commit"},
		{name: "begin failure", beginError: beginFailure, wantError: beginFailure, sequence: "begin"},
		{name: "query failure", reads: 1, queryError: queryFailure, wantError: queryFailure, sequence: "begin,callback,query,rollback"},
		{name: "callback failure", callbackError: callbackFailure, wantError: callbackFailure, sequence: "begin,callback,rollback"},
		{name: "commit failure", commitError: commitFailure, wantError: commitFailure, sequence: "begin,callback,commit"},
		{name: "callback and rollback failures", callbackError: callbackFailure, rollbackError: rollbackFailure, wantError: callbackFailure, sequence: "begin,callback,rollback"},
		{name: "query and rollback failures", reads: 1, queryError: queryFailure, rollbackError: rollbackFailure, wantError: queryFailure, sequence: "begin,callback,query,rollback"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 指定只读事务故障，验证回调、原上下文值及同一事务连接。
			script := &loginTransactionScript{beginError: testCase.beginError, queryError: testCase.queryError, commitError: testCase.commitError, rollbackError: testCase.rollbackError}
			store, connector := fileEntrySnapshotTestStore(t, script)
			type contextKey struct{}
			ctx := context.WithValue(context.Background(), contextKey{}, "snapshot-value")
			var receivedContext context.Context
			var receivedStore *Store
			err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
				script.record("callback")
				receivedContext, receivedStore = txContext, txStore
				if txContext.Value(contextKey{}) != "snapshot-value" || txContext != connector.beginContext || txStore == store {
					t.Error("回调未收到准确事务上下文或独立 Store")
				}
				if _, exists := txStore.database.Statement.ConnPool.(*sql.Tx); !exists || txStore.loginUserID != 0 || txStore.authenticatedActor != nil || txStore.refreshSession != nil {
					t.Error("快照 Store 的事务绑定或身份状态错误")
				}
				if deadline, exists := txContext.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("快照事务缺少五秒上限")
				}
				for range testCase.reads {
					entry, err := txStore.FindUserFileEntry(txContext, 7, 19)
					if err != nil {
						return err
					}
					if entry == nil || entry.ID != 19 {
						t.Error("快照未复用目录项读取方法")
					}
				}
				return testCase.callbackError
			})

			// 2. 核对显式事务选项、错误原因、提交/回滚顺序及结束后的资源状态。
			if connector.options.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) || !connector.options.ReadOnly || script.sequence() != testCase.sequence {
				t.Fatal("事务选项或生命周期错误", script.sequence())
			}
			if (testCase.wantError == nil && err != nil) || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
				t.Fatal("事务错误原因丢失", err)
			}
			if testCase.rollbackError != nil && !errors.Is(err, rollbackFailure) {
				t.Fatal("回滚失败原因未保留")
			}
			if receivedContext != nil && !errors.Is(receivedContext.Err(), context.Canceled) {
				t.Fatal("事务返回后未取消内部上下文")
			}
			if receivedStore != nil {
				closedError := error(sql.ErrTxDone)
				if testCase.rollbackError != nil {
					closedError = testCase.rollbackError
				}
				if _, err := receivedStore.FindUserFileEntry(context.Background(), 7, 19); !errors.Is(err, closedError) || script.sequence() != testCase.sequence {
					t.Fatal("结束后的事务 Store 错误状态异常或再次访问数据库", err)
				}
			}
			pool, err := store.database.DB()
			if err != nil || pool.PingContext(context.Background()) != nil {
				t.Fatal("快照入口关闭了共享连接池", err)
			}
		})
	}
}

func TestWithFileEntrySnapshotInputAndNesting(t *testing.T) {
	for _, name := range []string{"nil context", "nil callback", "cancelled", "expired"} {
		t.Run(name, func(t *testing.T) {
			script := &loginTransactionScript{}
			store, _ := fileEntrySnapshotTestStore(t, script)
			var ctx context.Context = context.Background()
			action := func(context.Context, *Store) error { t.Error("非法输入执行了回调"); return nil }
			var wantError error
			switch name {
			case "nil context":
				ctx = nil
			case "nil callback":
				action = nil
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, wantError = cancelled, context.Canceled
			case "expired":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				ctx, wantError = expired, context.DeadlineExceeded
			}
			err := store.WithFileEntrySnapshot(ctx, action)
			if err == nil || (wantError != nil && !errors.Is(err, wantError)) || script.sequence() != "" {
				t.Fatal("非法入口开启了事务或丢失错误", err)
			}
		})
	}
	t.Run("nested snapshot rejected", func(t *testing.T) {
		script := &loginTransactionScript{}
		store, _ := fileEntrySnapshotTestStore(t, script)
		err := store.WithFileEntrySnapshot(context.Background(), func(ctx context.Context, txStore *Store) error {
			return txStore.WithFileEntrySnapshot(ctx, func(context.Context, *Store) error { t.Error("执行了嵌套回调"); return nil })
		})
		if err == nil || script.sequence() != "begin,rollback" {
			t.Fatal("嵌套快照开启了额外事务", err)
		}
	})
	t.Run("snapshot inside write transaction rejected", func(t *testing.T) {
		script := &loginTransactionScript{}
		store, _ := fileEntrySnapshotTestStore(t, script)
		transaction := store.database.Begin()
		if transaction.Error != nil {
			t.Fatal(transaction.Error)
		}
		defer transaction.Rollback()
		txStore := &Store{database: transaction}
		if err := txStore.WithFileEntrySnapshot(context.Background(), func(context.Context, *Store) error { t.Error("执行了嵌套回调"); return nil }); err == nil || script.sequence() != "begin" {
			t.Fatal("写事务内允许开启快照", err)
		}
	})
}

func TestWithFileEntrySnapshotPanic(t *testing.T) {
	script := &loginTransactionScript{}
	store, _ := fileEntrySnapshotTestStore(t, script)
	payload := &struct{ message string }{message: "fixture panic"}
	func() {
		defer func() {
			if recover() != payload {
				t.Error("快照回调原始 panic 丢失")
			}
		}()
		_ = store.WithFileEntrySnapshot(context.Background(), func(context.Context, *Store) error { panic(payload) })
	}()
	if script.sequence() != "begin,rollback" {
		t.Fatal("panic 未触发回滚")
	}
}

func TestWithFileEntrySnapshotCancellation(t *testing.T) {
	for _, name := range []string{"callback cancels", "query cancels", "caller deadline", "default deadline"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "caller deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 30*time.Millisecond)
				defer release()
			}
			script := &loginTransactionScript{}
			if name == "query cancels" {
				script.beforeQuery = func(context.Context) { cancel() }
			}
			store, _ := fileEntrySnapshotTestStore(t, script)
			err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
				switch name {
				case "callback cancels":
					cancel()
				case "query cancels":
					_, err := txStore.FindUserFileEntry(txContext, 7, 19)
					return err
				default:
					<-txContext.Done()
				}
				return nil
			})
			wantError := error(context.Canceled)
			if strings.Contains(name, "deadline") {
				wantError = context.DeadlineExceeded
			}
			if !errors.Is(err, wantError) {
				t.Fatal("取消或超时错误丢失", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消后事务未回滚")
			}
			if strings.Contains(script.sequence(), "commit") {
				t.Fatal("取消后仍提交事务")
			}
		})
	}
}
