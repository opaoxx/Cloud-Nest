package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type lockedDeviceRows struct {
	values []driver.Value
	read   bool
}

func (*lockedDeviceRows) Columns() []string { return []string{"id", "user_id", "revoked_at"} }
func (*lockedDeviceRows) Close() error      { return nil }
func (rows *lockedDeviceRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestCreateLoginSessionResults(t *testing.T) {
	queryFailure, insertFailure, idFailure := errors.New("query fault"), errors.New("insert fault"), errors.New("id fault")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private refresh hash"}
	foreignKey := &mysql.MySQLError{Number: 1452, Message: "foreign key fault"}
	inputExpiry := time.Now().Add(time.Hour).In(time.FixedZone("UTC+8", 8*60*60))
	wantExpiry := inputExpiry.UTC().Truncate(time.Microsecond)
	tokenHash := sha256.Sum256([]byte("fixture-token"))
	for _, testCase := range []struct {
		name                      string
		deviceID                  uint64
		values                    []driver.Value
		queryError, insertError   error
		result                    deviceInsertResult
		wantError                 error
		wantInsert, wantCommitted bool
	}{
		{name: "created", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, result: deviceInsertResult{id: 101, rows: 1}, wantInsert: true, wantCommitted: true},
		{name: "unsigned device id", deviceID: ^uint64(0), values: []driver.Value{"18446744073709551615", int64(7), nil}, result: deviceInsertResult{id: 102, rows: 1}, wantInsert: true, wantCommitted: true},
		{name: "device missing", deviceID: 11, wantError: domain.ErrDeviceNotFound},
		{name: "device revoked", deviceID: 11, values: []driver.Value{int64(11), int64(7), time.Now().UTC()}, wantError: domain.ErrDeviceRevoked},
		{name: "wrong owner", deviceID: 11, values: []driver.Value{int64(11), int64(8), nil}, wantError: domain.ErrInvalidDeviceData},
		{name: "wrong device", deviceID: 11, values: []driver.Value{int64(12), int64(7), nil}, wantError: domain.ErrInvalidDeviceData},
		{name: "device query fault", deviceID: 11, queryError: queryFailure, wantError: queryFailure},
		{name: "duplicate hash", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, insertError: duplicate, wantError: domain.ErrRefreshTokenConflict, wantInsert: true},
		{name: "wrapped duplicate", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, insertError: fmt.Errorf("wrapper: %w", duplicate), wantError: domain.ErrRefreshTokenConflict, wantInsert: true},
		{name: "foreign key fault", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, insertError: foreignKey, wantError: foreignKey, wantInsert: true},
		{name: "database fault", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, insertError: insertFailure, wantError: insertFailure, wantInsert: true},
		{name: "id read fault", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, result: deviceInsertResult{rows: 1, idError: idFailure}, wantError: idFailure, wantInsert: true},
		{name: "zero id", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, result: deviceInsertResult{rows: 1}, wantError: domain.ErrInvalidSessionData, wantInsert: true},
		{name: "zero rows", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, result: deviceInsertResult{id: 101}, wantError: domain.ErrInvalidSessionData, wantInsert: true},
		{name: "multiple rows", deviceID: 11, values: []driver.Value{int64(11), int64(7), nil}, result: deviceInsertResult{id: 101, rows: 2}, wantError: domain.ErrInvalidSessionData, wantInsert: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			script := &loginTransactionScript{status: "ACTIVE"}
			script.deviceQuery = func(ctx context.Context, query string, arguments []driver.NamedValue) (driver.Rows, error) {
				if strings.ReplaceAll(query, "`", "") != "SELECT id,user_id,revoked_at FROM user_devices WHERE user_id = ? AND id = ? LIMIT ? FOR UPDATE" || len(arguments) != 3 || arguments[0].Value != uint64(7) || arguments[1].Value != testCase.deviceID {
					t.Error("设备锁查询列、归属或参数不正确")
				}
				return &lockedDeviceRows{values: testCase.values}, testCase.queryError
			}
			script.insert = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				if strings.ReplaceAll(statement, "`", "") != "INSERT INTO auth_sessions (user_id,device_id,refresh_token_hash,expires_at) VALUES (?,?,?,?)" || len(arguments) != 4 {
					t.Fatal("会话 INSERT 列清单不正确")
				}
				if arguments[0].Value != uint64(7) || arguments[1].Value != testCase.deviceID || !bytes.Equal(arguments[2].Value.([]byte), tokenHash[:]) || arguments[3].Value != wantExpiry {
					t.Error("归属、二进制摘要或 UTC 微秒到期时间错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("写入缺少期限")
				}
				return testCase.result, testCase.insertError
			}
			store := loginTransactionTestStore(t, script)
			var session *domain.LoginSession
			err := store.WithLoginUserTransaction(context.Background(), 7, func(txContext context.Context, txStore *Store) error {
				var err error
				session, err = txStore.CreateLoginSession(txContext, testCase.deviceID, tokenHash, inputExpiry)
				return err
			})
			sequence := "begin,lock,device"
			if testCase.wantInsert {
				sequence += ",insert"
			}
			if testCase.wantCommitted {
				sequence += ",commit"
				if err != nil || session == nil || session.ID != uint64(testCase.result.id) || session.UserID != 7 || session.DeviceID != testCase.deviceID || session.ExpiresAt != wantExpiry {
					t.Fatalf("会话结果不正确: %v", err)
				}
			} else {
				sequence += ",rollback"
				if session != nil || !errors.Is(err, testCase.wantError) {
					t.Fatalf("失败结果或错误链不正确: %v", err)
				}
				if testCase.wantError == domain.ErrRefreshTokenConflict && strings.Contains(err.Error(), "private") {
					t.Error("重复摘要错误泄漏驱动敏感值")
				}
			}
			if script.sequence() != sequence {
				t.Fatalf("执行顺序不正确: %s", script.sequence())
			}
		})
	}
}

func TestCreateLoginSessionInput(t *testing.T) {
	script := &loginTransactionScript{status: "ACTIVE"}
	store := loginTransactionTestStore(t, script)
	var tokenHash [sha256.Size]byte
	future := time.Now().Add(time.Hour)
	for _, candidate := range []*Store{store, {database: store.database, loginUserID: 7}} {
		if session, err := candidate.CreateLoginSession(context.Background(), 11, tokenHash, future); session != nil || err == nil || script.sequence() != "" {
			t.Fatal("事务外访问了数据库")
		}
	}
	err := store.WithLoginUserTransaction(context.Background(), 7, func(txContext context.Context, txStore *Store) error {
		canceled, cancel := context.WithCancel(txContext)
		cancel()
		for _, testCase := range []struct {
			ctx       context.Context
			store     *Store
			deviceID  uint64
			expiresAt time.Time
			wantError error
		}{
			{store: txStore, deviceID: 11, expiresAt: future},
			{ctx: txContext, store: &Store{database: txStore.database}, deviceID: 11, expiresAt: future},
			{ctx: txContext, store: txStore, expiresAt: future},
			{ctx: txContext, store: txStore, deviceID: 11},
			{ctx: txContext, store: txStore, deviceID: 11, expiresAt: time.Now().Add(-time.Hour)},
			{ctx: txContext, store: txStore, deviceID: 11, expiresAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
			{ctx: canceled, store: txStore, deviceID: 11, expiresAt: future, wantError: context.Canceled},
		} {
			if session, err := testCase.store.CreateLoginSession(testCase.ctx, testCase.deviceID, tokenHash, testCase.expiresAt); session != nil || err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
				t.Error("无效参数或取消没有被拒绝")
			}
		}
		return nil
	})
	if err != nil || script.sequence() != "begin,lock,commit" {
		t.Fatal("无效输入仍触发设备查询或插入")
	}
}

func TestCreateLoginSessionCancellationAndExpiry(t *testing.T) {
	for _, phase := range []string{"query", "insert", "after insert", "expired while locking"} {
		t.Run(phase, func(t *testing.T) {
			script := &loginTransactionScript{status: "ACTIVE"}
			store := loginTransactionTestStore(t, script)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			expiresAt := time.Now().Add(time.Hour)
			script.deviceQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "query" {
					cancel()
					return nil, queryContext.Err()
				}
				if phase == "expired while locking" {
					<-time.After(time.Until(expiresAt) + time.Millisecond)
				}
				return &lockedDeviceRows{values: []driver.Value{int64(11), int64(7), nil}}, nil
			}
			script.insert = func(writeContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if phase == "after insert" {
					cancel()
					return deviceInsertResult{id: 101, rows: 1}, nil
				}
				<-writeContext.Done()
				return nil, writeContext.Err()
			}
			err := store.WithLoginUserTransaction(ctx, 7, func(txContext context.Context, txStore *Store) error {
				if phase == "expired while locking" {
					expiresAt = time.Now().Add(100 * time.Millisecond)
				}
				session, err := txStore.CreateLoginSession(txContext, 11, [sha256.Size]byte{}, expiresAt)
				if session != nil {
					t.Error("取消或到期仍返回会话")
				}
				return err
			})
			if err == nil || strings.Contains(script.sequence(), "commit") {
				t.Fatal("失败仍提交会话")
			}
			if phase == "expired while locking" {
				if strings.Contains(script.sequence(), "insert") {
					t.Error("等锁后已到期仍执行插入")
				}
			} else {
				wantError := error(context.Canceled)
				if phase == "insert" {
					wantError = context.DeadlineExceeded
				}
				if !errors.Is(err, wantError) {
					t.Error("取消原因丢失")
				}
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Error("失败事务没有结束")
			}
		})
	}
}
