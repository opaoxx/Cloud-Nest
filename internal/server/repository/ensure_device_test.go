package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestEnsureLoginDeviceOutcomes(t *testing.T) {
	const installationID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	revokedAt := time.Date(2026, 11, 10, 3, 0, 0, 123456000, time.UTC)
	queryFailure, restoreFailure := errors.New("query fixture fault"), errors.New("restore fixture fault")
	syncFailure := errors.New("sync fixture fault")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private installation"}
	for _, testCase := range []struct {
		name, sequence                                   string
		record                                           []driver.Value
		queryError, insertError, restoreError, syncError error
		restoredRows                                     int64
		wantError                                        error
		wantID                                           uint64
	}{
		{name: "create missing", sequence: "begin,lock,device,insert,commit", wantID: 101},
		{name: "reuse existing", sequence: "begin,lock,device,commit", record: []driver.Value{int64(11), int64(7), installationID, "用户改过的名称", "WINDOWS", nil}, wantID: 11},
		{name: "restore existing", sequence: "begin,lock,device,restore,sync_device,commit", record: []driver.Value{int64(11), int64(7), installationID, "用户改过的名称", "WINDOWS", revokedAt}, restoredRows: 1, wantID: 11},
		{name: "unsigned existing id", sequence: "begin,lock,device,restore,sync_device,commit", record: []driver.Value{"18446744073709551615", int64(7), installationID, "用户改过的名称", "WINDOWS", revokedAt}, restoredRows: 1, wantID: ^uint64(0)},
		{name: "query failure", sequence: "begin,lock,device,rollback", queryError: queryFailure, wantError: queryFailure},
		{name: "create conflict", sequence: "begin,lock,device,insert,rollback", insertError: duplicate, wantError: domain.ErrDeviceAlreadyExists},
		{name: "restore failure", sequence: "begin,lock,device,restore,rollback", record: []driver.Value{int64(11), int64(7), installationID, "original", "WEB", revokedAt}, restoreError: restoreFailure, wantError: restoreFailure},
		{name: "sync failure", sequence: "begin,lock,device,restore,sync_device,rollback", record: []driver.Value{int64(11), int64(7), installationID, "original", "WEB", revokedAt},
			restoredRows: 1, syncError: syncFailure, wantError: syncFailure},
		{name: "restore zero rows", sequence: "begin,lock,device,restore,rollback", record: []driver.Value{int64(11), int64(7), installationID, "original", "WEB", revokedAt}, wantError: domain.ErrInvalidDeviceData},
		{name: "restore multiple rows", sequence: "begin,lock,device,restore,rollback", record: []driver.Value{int64(11), int64(7), installationID, "original", "WEB", revokedAt}, restoredRows: 2, wantError: domain.ErrInvalidDeviceData},
		{name: "zero device id", sequence: "begin,lock,device,rollback", record: []driver.Value{int64(0), int64(7), installationID, "original", "WEB", nil}, wantError: domain.ErrInvalidDeviceData},
		{name: "wrong owner", sequence: "begin,lock,device,rollback", record: []driver.Value{int64(11), int64(8), installationID, "original", "WEB", nil}, wantError: domain.ErrInvalidDeviceData},
		{name: "wrong installation", sequence: "begin,lock,device,rollback", record: []driver.Value{int64(11), int64(7), "another-installation", "original", "WEB", nil}, wantError: domain.ErrInvalidDeviceData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			script := &loginTransactionScript{status: "ACTIVE"}
			script.deviceQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if len(arguments) != 3 || arguments[0].Value != uint64(7) || arguments[1].Value != installationID {
					t.Error("查询归属或安装标识没有绑定到当前用户")
				}
				if testCase.queryError != nil {
					return nil, testCase.queryError
				}
				rows := &deviceRows{}
				if testCase.record != nil {
					rows.records = [][]driver.Value{testCase.record}
				}
				return rows, nil
			}
			script.insert = func(_ context.Context, _ string, arguments []driver.NamedValue) (driver.Result, error) {
				if len(arguments) != 4 || arguments[0].Value != uint64(7) || arguments[1].Value != installationID || arguments[2].Value != "新设备" || arguments[3].Value != "ANDROID" {
					t.Error("创建没有沿用传入字段和当前用户")
				}
				return deviceInsertResult{id: 101, rows: 1}, testCase.insertError
			}
			script.restore = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				if statement != "UPDATE user_devices SET revoked_at = NULL, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND revoked_at IS NOT NULL" || len(arguments) != 2 || arguments[0].Value != uint64(7) {
					t.Error("恢复没有限定用户与设备，或更新了额外字段")
				}
				if testCase.wantID != 0 && arguments[1].Value != testCase.wantID {
					t.Error("恢复时设备 ID 改变或发生截断")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("恢复操作缺少期限")
				}
				return driver.RowsAffected(testCase.restoredRows), testCase.restoreError
			}
			script.syncDeviceQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				expectedID := testCase.wantID
				if expectedID == 0 {
					expectedID = 11
				}
				if statement != "SELECT status, cursor_generation, ack_sequence FROM sync_devices WHERE user_id = ? AND device_id = ? FOR UPDATE" ||
					len(arguments) != 2 || arguments[0].Value != uint64(7) || arguments[1].Value != expectedID {
					t.Error("同步锁读未绑定重新激活的账号和设备 ID")
				}
				return &revokedSyncDeviceRows{&idempotencyRecordRows{}}, testCase.syncError
			}
			store := loginTransactionTestStore(t, script)
			var device *domain.LoginDevice
			err := store.WithLoginUserTransaction(context.Background(), 7, func(txContext context.Context, txStore *Store) error {
				var err error
				device, err = txStore.EnsureLoginDevice(txContext, installationID, "新设备", "ANDROID")
				return err
			})
			if script.sequence() != testCase.sequence {
				t.Fatalf("执行顺序错误: %s", script.sequence())
			}
			if testCase.wantError != nil {
				if device != nil || !errors.Is(err, testCase.wantError) {
					t.Fatalf("错误分类或结果错误: %v", err)
				}
				return
			}
			if err != nil || device == nil || device.ID != testCase.wantID || device.UserID != 7 || device.InstallationID != installationID || device.RevokedAt != nil {
				t.Fatalf("设备准备结果错误: %+v, %v", device, err)
			}
			if testCase.record != nil && (device.Name != "用户改过的名称" || device.Platform != "WINDOWS") {
				t.Error("已有名称或平台被登录输入覆盖")
			}
		})
	}
}

func TestEnsureLoginDeviceTransactionAndContext(t *testing.T) {
	script := &loginTransactionScript{status: "ACTIVE"}
	store := loginTransactionTestStore(t, script)
	for _, candidate := range []*Store{store, {database: store.database, loginUserID: 7}} {
		if device, err := candidate.EnsureLoginDevice(context.Background(), "private-installation", "name", "WEB"); device != nil || err == nil || script.sequence() != "" {
			t.Fatal("事务外执行了设备准备")
		}
	}
	err := store.WithLoginUserTransaction(context.Background(), 7, func(txContext context.Context, txStore *Store) error {
		for _, candidate := range []struct {
			store *Store
			ctx   context.Context
		}{
			{store: txStore},
			{store: &Store{database: txStore.database}, ctx: txContext},
		} {
			if device, err := candidate.store.EnsureLoginDevice(candidate.ctx, "private-installation", "name", "WEB"); device != nil || err == nil || strings.Contains(err.Error(), "private") {
				t.Error("缺少上下文或用户锁标记仍被接受")
			}
		}
		canceled, cancel := context.WithCancel(txContext)
		cancel()
		if device, err := txStore.EnsureLoginDevice(canceled, "private-installation", "name", "WEB"); device != nil || !errors.Is(err, context.Canceled) {
			t.Error("预先取消仍执行设备准备")
		}
		return nil
	})
	if err != nil || script.sequence() != "begin,lock,commit" {
		t.Fatal("无效输入触发了查询或写入")
	}
}

func TestEnsureLoginDeviceCancellation(t *testing.T) {
	for _, phase := range []string{"query", "restore", "after restore"} {
		t.Run(phase, func(t *testing.T) {
			script := &loginTransactionScript{status: "ACTIVE"}
			store := loginTransactionTestStore(t, script)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			script.deviceQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "query" {
					cancel()
					return nil, domain.ErrDeviceNotFound
				}
				return &deviceRows{records: [][]driver.Value{{int64(11), int64(7), "28ee758a-0727-42de-ae7d-3bb33155e148", "original", "WEB", time.Now().UTC()}}}, nil
			}
			script.restore = func(writeContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if phase == "after restore" {
					cancel()
					return driver.RowsAffected(1), nil
				}
				<-writeContext.Done()
				return nil, writeContext.Err()
			}
			wantError := error(context.Canceled)
			if phase == "restore" {
				wantError = context.DeadlineExceeded
			}
			err := store.WithLoginUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
				device, err := txStore.EnsureLoginDevice(txContext, "28ee758a-0727-42de-ae7d-3bb33155e148", "new name", "ANDROID")
				if device != nil {
					t.Error("取消仍返回设备")
				}
				return err
			})
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") || strings.Contains(script.sequence(), "insert") {
				t.Fatalf("取消原因丢失或继续创建/提交: %v", err)
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("取消事务没有结束")
			}
		})
	}
}
