package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCreatePublishedFileEntryReadbackTime(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		insertAtExpiry bool
		wantError      error
	}{
		{name: "insert within lease"},
		{name: "insert exactly at lease expiry", insertAtExpiry: true, wantError: domain.ErrUploadTaskBusy},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			task, taskValues, _, _, now := uploadObjectReplacementFixture()
			task.Status, taskValues[14] = domain.UploadStatusVerifying, "VERIFYING"
			object, objectValues := storedObjectReadFixture()
			task.ExpectedSHA256, taskValues[10] = object.VerifiedSHA256, object.VerifiedSHA256[:]
			source := domain.UserDevice{ID: task.DeviceID, UserID: task.UserID, Name: "fixture", Platform: "WEB", CreatedAt: now}
			input := domain.CreatePublishedFileEntryInput{
				ParentID: task.TargetParentID, Name: "文档.pdf", NameKey: []byte("文档.pdf"),
				Category: "DOCUMENT", ObjectID: object.ID, SizeBytes: object.FileSize,
				SourceDeviceID: source.ID, SourceDeviceName: source.Name,
			}
			insertedAt := now.Add(time.Microsecond)
			if scenario.insertAtExpiry {
				insertedAt = *task.LeaseUntil
			}
			script := &loginTransactionScript{status: "ACTIVE"}
			script.uploadQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &uploadTaskReadRows{&idempotencyRecordRows{values: taskValues}}, nil
			}
			script.objectQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &storedObjectReadRows{&idempotencyRecordRows{values: objectValues}}, nil
			}
			script.deviceQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &userDeviceRows{records: [][]driver.Value{userDeviceValues(source)}}, nil
			}
			script.leaseClockQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				return &uploadLeaseClockRows{&idempotencyRecordRows{values: []driver.Value{now}}}, nil
			}
			script.entryQuery = func(_ context.Context, statement string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(statement, "active_name_key = ?") {
					return &fileEntryRows{values: []driver.Value{
						int64(41), task.UserID, input.ParentID, input.Name, "FILE", input.SizeBytes,
						int64(1), "ACTIVE", insertedAt, insertedAt, insertedAt,
					}}, nil
				}
				return &fileEntryRows{values: []driver.Value{
					input.ParentID, task.UserID, nil, "", "DIR", int64(0), int64(1), "ACTIVE", now, now, now,
				}}, nil
			}
			script.insert = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				if !strings.HasPrefix(statement, "INSERT INTO `file_entries` ") || len(arguments) != 14 ||
					arguments[8].Value != nil || arguments[13].Value != now {
					t.Fatal("预期使用映射和数据库时间插入文件", statement, arguments)
				}
				return driver.RowsAffected(1), nil
			}
			store := loginTransactionTestStore(t, script)
			if entry, err := store.CreatePublishedFileEntry(t.Context(), &task, input); err == nil || entry != nil {
				t.Fatal("事务外调用应被拒绝", entry, err)
			}
			err := store.withActiveUserTransaction(t.Context(), task.UserID, func(txContext context.Context, txStore *Store) error {
				entry, err := txStore.CreatePublishedFileEntry(txContext, &task, input)
				if scenario.wantError == nil && (entry == nil || entry.ID != 41) || scenario.wantError != nil && entry != nil {
					t.Fatal("写入回读结果错误", entry, err)
				}
				return err
			})
			expectedSequence := "begin,lock,upload,entry,object,device,lease_clock,insert,entry,commit"
			if scenario.insertAtExpiry {
				expectedSequence = strings.TrimSuffix(expectedSequence, "commit") + "rollback"
			}
			if !errors.Is(err, scenario.wantError) || script.sequence() != expectedSequence {
				t.Fatal("实际插入时间校验或事务顺序错误", err, script.sequence())
			}
		})
	}
}
