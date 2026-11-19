package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSettlePublishedUploadQuotaResults(t *testing.T) {
	writeFailure := errors.New("fixture quota write failure")
	for _, scenario := range []struct {
		name       string
		rows       int64
		zero       bool
		expiresNow bool
		writeError error
		wantError  error
		wantUpdate bool
	}{
		{name: "transfer with fenced SQL", rows: 2, wantUpdate: true},
		{name: "zero bytes", zero: true},
		{name: "zero bytes with expired lease", zero: true, expiresNow: true, wantError: domain.ErrUploadTaskBusy},
		{name: "lease expires before conditional write", wantUpdate: true, wantError: domain.ErrUploadTaskBusy},
		{name: "partial update cannot commit", rows: 1, wantUpdate: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "database write failure", wantUpdate: true, writeError: writeFailure, wantError: writeFailure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			task, values, _, _, now := uploadObjectReplacementFixture()
			task.Status, values[14] = domain.UploadStatusVerifying, "VERIFYING"
			if scenario.zero {
				task.FileSize, task.ReservedBytes = 0, 0
				values[11], values[16] = "0", "0"
			}
			if scenario.expiresNow {
				values[18] = now
			}
			const quota, used, reserved = uint64(4096), uint64(1024), uint64(2048)
			script := &loginTransactionScript{status: "ACTIVE"}
			script.uploadQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.quotaQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				row := []driver.Value{"7", "ACTIVE", strconv.FormatUint(quota, 10), strconv.FormatUint(used, 10), strconv.FormatUint(reserved, 10)}
				return &uploadQuotaRows{&idempotencyRecordRows{values: row}}, nil
			}
			script.leaseClockQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{now}}}, nil
			}
			script.settleUploadQuota = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				for _, fragment := range []string{
					"upload_task.reserved_bytes = 0", "CAST(upload_task.lease_owner AS BINARY) = ?", "upload_task.lease_version = ?",
					"upload_task.lease_until > UTC_TIMESTAMP(6)", "upload_task.expires_at > UTC_TIMESTAMP(6)",
					"upload_task.reserved_bytes = upload_task.file_size",
				} {
					if !strings.Contains(statement, fragment) {
						t.Fatal("结算 SQL 缺少保护条件", fragment)
					}
				}
				actual := make([]any, 0, len(arguments))
				for _, argument := range arguments {
					actual = append(actual, argument.Value)
				}
				want := []any{used + task.FileSize, reserved - task.FileSize, task.UserID, quota, used, reserved,
					task.ID, task.StagedObjectID, *task.LeaseOwner, task.LeaseVersion, task.FileSize}
				if !scenario.wantUpdate || !reflect.DeepEqual(actual, want) {
					t.Fatal("结算参数或更新次数错误", actual)
				}
				return driver.RowsAffected(scenario.rows), scenario.writeError
			}
			store := loginTransactionTestStore(t, script)
			if err := store.SettlePublishedUploadQuota(t.Context(), &task); err == nil {
				t.Fatal("事务外结算应被拒绝")
			}
			err := store.withActiveUserTransaction(t.Context(), task.UserID, func(txContext context.Context, txStore *Store) error {
				return txStore.SettlePublishedUploadQuota(txContext, &task)
			})
			sequence := "begin,lock,upload,quota,lease_clock"
			if scenario.wantUpdate {
				sequence += ",settle_quota"
			}
			if scenario.wantError == nil {
				sequence += ",commit"
			} else {
				sequence += ",rollback"
			}
			if !errors.Is(err, scenario.wantError) || script.sequence() != sequence {
				t.Fatal("结算结果或事务顺序错误", err, script.sequence())
			}
		})
	}
}
