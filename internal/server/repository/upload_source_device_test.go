package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUploadSourceDeviceResults(t *testing.T) {
	queryFailure := errors.New("fixture device query failure")
	for _, scenario := range []struct {
		name        string
		deviceID    uint64
		revoked     bool
		missing     bool
		rowChanges  map[int]driver.Value
		returnError error
		wantError   error
	}{
		{name: "active source", deviceID: 11},
		{name: "revoked source with full unsigned ID", deviceID: math.MaxUint64, revoked: true},
		{name: "missing", deviceID: 11, missing: true, wantError: domain.ErrDeviceNotFound},
		{name: "query failure", deviceID: 11, returnError: queryFailure, wantError: queryFailure},
		{name: "wrong device", deviceID: 11, rowChanges: map[int]driver.Value{0: "12"}, wantError: domain.ErrInvalidDeviceData},
		{name: "wrong owner", deviceID: 11, rowChanges: map[int]driver.Value{1: "8"}, wantError: domain.ErrInvalidDeviceData},
		{name: "invalid name", deviceID: 11, rowChanges: map[int]driver.Value{2: " "}, wantError: domain.ErrInvalidDeviceData},
		{name: "missing creation time", deviceID: 11, rowChanges: map[int]driver.Value{4: time.Time{}}, wantError: domain.ErrInvalidDeviceData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备来源设备记录及本组异常或缺失。
			createdAt := time.Date(2026, 11, 19, 8, 0, 0, 123456000, time.UTC)
			want := domain.UserDevice{ID: scenario.deviceID, UserID: 7, Name: " 学习设备 🌱 ", Platform: "WEB", CreatedAt: createdAt}
			if scenario.revoked {
				seenAt, revokedAt := createdAt.Add(time.Minute), createdAt.Add(time.Hour)
				want.LastSeenAt, want.RevokedAt = &seenAt, &revokedAt
			}
			values := userDeviceValues(want)
			for column, value := range scenario.rowChanges {
				values[column] = value
			}
			var records [][]driver.Value
			if !scenario.missing {
				records = append(records, values)
			}

			// 2. 核对真实查询使用事务用户和任务设备 ID。
			script := &loginTransactionScript{status: "ACTIVE"}
			script.deviceQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				const expectedSQL = "SELECT id,user_id,name,platform,created_at,last_seen_at,revoked_at FROM user_devices WHERE user_id = ? AND id = ? LIMIT ?"
				if strings.ReplaceAll(statement, "`", "") != expectedSQL || len(arguments) != 3 ||
					arguments[0].Value != want.UserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Fatal("来源设备查询范围或投影错误", statement, arguments)
				}
				return &userDeviceRows{records: records}, scenario.returnError
			}
			store := loginTransactionTestStore(t, script)
			ctx := context.Background()
			task := &domain.UploadTask{ID: 19, UserID: want.UserID, DeviceID: want.ID}
			if device, err := store.FindUploadSourceDevice(ctx, task); device != nil || err == nil {
				t.Fatal("事务外读取应被拒绝", err)
			}

			// 3. 核对取消、跨账号任务及实际设备结果。
			err := store.withActiveUserTransaction(ctx, task.UserID, func(txContext context.Context, txStore *Store) error {
				cancelled, cancel := context.WithCancel(txContext)
				cancel()
				if device, err := txStore.FindUploadSourceDevice(cancelled, task); device != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("未保留取消原因", err)
				}
				foreignTask := *task
				foreignTask.UserID++
				if device, err := txStore.FindUploadSourceDevice(txContext, &foreignTask); device != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
					t.Fatal("跨账号任务应被拒绝", err)
				}
				device, err := txStore.FindUploadSourceDevice(txContext, task)
				if scenario.wantError == nil && !reflect.DeepEqual(device, &want) || scenario.wantError != nil && device != nil {
					t.Fatal("来源设备结果错误", device, err)
				}
				return err
			})
			expectedSequence := "begin,lock,device,commit"
			if scenario.wantError != nil {
				expectedSequence = "begin,lock,device,rollback"
			}
			if !errors.Is(err, scenario.wantError) || script.sequence() != expectedSequence {
				t.Fatal("来源设备错误或事务顺序错误", err, script.sequence())
			}
		})
	}
}
