package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// deviceInsertResult 允许检验自增 ID 缺失或驱动无法读取结果时不会返回假成功。
type deviceInsertResult struct {
	id, rows int64
	idError  error
}

func (result deviceInsertResult) LastInsertId() (int64, error) { return result.id, result.idError }
func (result deviceInsertResult) RowsAffected() (int64, error) { return result.rows, nil }

func TestCreateLoginDeviceResults(t *testing.T) {
	const installationID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	insertFailure, idFailure := errors.New("private insertion fault"), errors.New("private result fault")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private duplicate installation"}
	foreignKey := &mysql.MySQLError{Number: 1452, Message: "private foreign key fault"}
	checkFailure := &mysql.MySQLError{Number: 3819, Message: "private check fault"}
	for _, testCase := range []struct {
		name, deviceName, platform string
		result                     deviceInsertResult
		insertError, wantError     error
		wantSuccess                bool
	}{
		{name: "web", deviceName: " 学习电脑 ", platform: "WEB", result: deviceInsertResult{id: 11, rows: 1}, wantSuccess: true},
		{name: "windows hundred unicode characters", deviceName: strings.Repeat("学", 100), platform: "WINDOWS", result: deviceInsertResult{id: 12, rows: 1}, wantSuccess: true},
		{name: "android quoted name", deviceName: "手机' OR 1=1 --", platform: "ANDROID", result: deviceInsertResult{id: 13, rows: 1}, wantSuccess: true},
		{name: "duplicate", insertError: duplicate, wantError: domain.ErrDeviceAlreadyExists},
		{name: "wrapped duplicate", insertError: fmt.Errorf("driver wrapper: %w", duplicate), wantError: domain.ErrDeviceAlreadyExists},
		{name: "foreign key stays internal", insertError: foreignKey, wantError: foreignKey},
		{name: "check stays internal", insertError: checkFailure, wantError: checkFailure},
		{name: "database fault", insertError: insertFailure, wantError: insertFailure},
		{name: "last insert id fault", result: deviceInsertResult{rows: 1, idError: idFailure}, wantError: idFailure},
		{name: "zero id", result: deviceInsertResult{rows: 1}},
		{name: "negative id", result: deviceInsertResult{id: -1, rows: 1}},
		{name: "zero rows", result: deviceInsertResult{id: 11}},
		{name: "multiple rows", result: deviceInsertResult{id: 11, rows: 2}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			script := &loginTransactionScript{status: "ACTIVE"}
			script.insert = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				// 只允许四列 INSERT；引号、空格和 Unicode 都应留在参数中。
				if strings.ReplaceAll(statement, "`", "") != "INSERT INTO user_devices (user_id,installation_id,name,platform) VALUES (?,?,?,?)" {
					t.Errorf("出现插入以外的写入或多余列: %s", statement)
				}
				values := make([]any, len(arguments))
				for index, argument := range arguments {
					values[index] = argument.Value
				}
				if !reflect.DeepEqual(values, []any{uint64(7), installationID, testCase.deviceName, testCase.platform}) {
					t.Error("用户归属或参数原文发生变化")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("写入没有期限")
				}
				return testCase.result, testCase.insertError
			}
			store := loginTransactionTestStore(t, script)
			var device *domain.LoginDevice
			err := store.WithLoginUserTransaction(context.Background(), 7, func(txContext context.Context, txStore *Store) error {
				var err error
				device, err = txStore.CreateLoginDevice(txContext, installationID, testCase.deviceName, testCase.platform)
				return err
			})
			if testCase.wantSuccess {
				want := &domain.LoginDevice{ID: uint64(testCase.result.id), UserID: 7, InstallationID: installationID, Name: testCase.deviceName, Platform: testCase.platform}
				if err != nil || !reflect.DeepEqual(device, want) || script.sequence() != "begin,lock,insert,commit" {
					t.Fatalf("创建/回填或事务生命周期错误: %v, events=%s", err, script.sequence())
				}
				return
			}
			if device != nil || err == nil || script.sequence() != "begin,lock,insert,rollback" {
				t.Fatalf("插入故障没有返回 nil 或回滚: %v, events=%s", err, script.sequence())
			}
			if testCase.wantError != nil && !errors.Is(err, testCase.wantError) {
				t.Fatalf("错误分类或原因丢失: %v", err)
			}
			if errors.Is(err, domain.ErrDeviceAlreadyExists) && strings.Contains(err.Error(), "private") {
				t.Fatal("重复错误回显了驱动中的安装标识")
			}
		})
	}
}

func TestCreateLoginDeviceRequiresLockedUserTransaction(t *testing.T) {
	script := &loginTransactionScript{status: "ACTIVE"}
	store := loginTransactionTestStore(t, script)
	for _, candidate := range []*Store{store, {database: store.database, loginUserID: 7}} {
		if device, err := candidate.CreateLoginDevice(context.Background(), "private-installation", "fixture", "WEB"); device != nil || err == nil || script.sequence() != "" {
			t.Fatal("普通连接池被允许直接创建设备")
		}
	}
	err := store.WithLoginUserTransaction(context.Background(), 7, func(txContext context.Context, txStore *Store) error {
		for _, testCase := range []struct {
			ctx   context.Context
			store *Store
		}{
			{ctx: nil, store: txStore},
			{ctx: txContext, store: &Store{database: txStore.database}},
		} {
			if device, err := testCase.store.CreateLoginDevice(testCase.ctx, "private-installation", "fixture", "WEB"); device != nil || err == nil || strings.Contains(err.Error(), "private") {
				t.Error("缺少上下文或用户锁标记仍被允许写入")
			}
		}
		canceledContext, cancel := context.WithCancel(txContext)
		cancel()
		if device, err := txStore.CreateLoginDevice(canceledContext, "private-installation", "fixture", "WEB"); device != nil || !errors.Is(err, context.Canceled) {
			t.Error("预先取消仍被允许写入")
		}
		return nil
	})
	if err != nil || script.sequence() != "begin,lock,commit" {
		t.Fatal("无效创建触发了 INSERT 或破坏了有效事务")
	}
}

func TestCreateLoginDeviceCancellation(t *testing.T) {
	for _, cancelAfterResult := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel after result=%v", cancelAfterResult), func(t *testing.T) {
			script := &loginTransactionScript{status: "ACTIVE"}
			store := loginTransactionTestStore(t, script)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			script.insert = func(writeContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if cancelAfterResult {
					cancel()
					return deviceInsertResult{id: 11, rows: 1}, nil
				}
				<-writeContext.Done()
				return nil, writeContext.Err()
			}
			wantError := error(context.DeadlineExceeded)
			if cancelAfterResult {
				wantError = context.Canceled
			}
			err := store.WithLoginUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
				device, err := txStore.CreateLoginDevice(txContext, "28ee758a-0727-42de-ae7d-3bb33155e148", "fixture", "WEB")
				if device != nil {
					t.Error("取消后仍返回新设备")
				}
				return err
			})
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") {
				t.Fatalf("取消原因丢失或仍提交: %v", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消没有结束事务")
			}
		})
	}
}
