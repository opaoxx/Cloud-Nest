package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCompletePublishedUploadResults(t *testing.T) {
	writeFailure := errors.New("fixture completion failed")
	for _, scenario := range []struct {
		name       string
		rows       int64
		writeError error
		readback   string
		wantError  error
	}{
		{name: "completed", rows: 1, readback: "normal"},
		{name: "no matching current records", wantError: domain.ErrUploadTaskBusy},
		{name: "unexpected rows", rows: 2, wantError: domain.ErrInvalidUploadTaskData},
		{name: "write failure", writeError: writeFailure, wantError: writeFailure},
		{name: "missing readback", rows: 1, readback: "missing", wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong readback name", rows: 1, readback: "wrong name", wantError: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			task, currentValues, _, _, now := uploadObjectReplacementFixture()
			task.Status, currentValues[14], currentValues[16] = domain.UploadStatusVerifying, "VERIFYING", "0"
			entry := &domain.FileEntry{ID: 31, UserID: task.UserID, ParentID: &task.TargetParentID, Name: "笔记 (1).PDF",
				FileSize: task.FileSize, EntryType: "FILE", Status: "ACTIVE", Revision: 1}
			const objectID uint64 = 29
			completedAt := now.Add(time.Microsecond)
			doneValues := append([]driver.Value(nil), currentValues...)
			doneValues[4], doneValues[7], doneValues[14], doneValues[15] = objectID, entry.Name, "COMPLETED", entry.ID
			doneValues[17], doneValues[18], doneValues[26], doneValues[27] = nil, nil, completedAt, completedAt
			if scenario.readback == "wrong name" {
				doneValues[7] = "other.pdf"
			}
			if scenario.readback == "missing" {
				doneValues = nil
			}

			script := &loginTransactionScript{status: "ACTIVE"}
			reads := 0
			script.uploadQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				reads++
				if reads == 1 {
					return &uploadTaskReadRows{&idempotencyRecordRows{values: currentValues}}, nil
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: doneValues}}, nil
			}
			script.completePublishedUpload = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				for _, condition := range []string{"upload_task.reserved_bytes = 0", "upload_task.lease_until > UTC_TIMESTAMP(6)",
					"upload_task.expires_at > UTC_TIMESTAMP(6)", "published_object.status = 'READY'", "result_entry.source_device_id = upload_task.device_id"} {
					if !strings.Contains(statement, condition) {
						t.Fatal("完成写入缺少保护条件", condition)
					}
				}
				var actual []any
				for _, argument := range arguments {
					actual = append(actual, argument.Value)
				}
				want := []any{entry.ID, task.UserID, task.ID, task.StagedObjectID, *task.LeaseOwner, task.LeaseVersion, entry.Name, task.FileSize, objectID}
				if !reflect.DeepEqual(actual, want) {
					t.Fatal("完成写入绑定值不符", actual)
				}
				return driver.RowsAffected(scenario.rows), scenario.writeError
			}
			store := loginTransactionTestStore(t, script)
			if result, err := store.CompletePublishedUpload(t.Context(), &task, objectID, entry); result != nil || err == nil {
				t.Fatal("事务外完成应被拒绝", err)
			}
			err := store.withActiveUserTransaction(t.Context(), task.UserID, func(txContext context.Context, txStore *Store) error {
				result, err := txStore.CompletePublishedUpload(txContext, &task, objectID, entry)
				if scenario.wantError == nil && (result == nil || result.CompletedAt == nil || !result.CompletedAt.Equal(completedAt)) ||
					scenario.wantError != nil && result != nil {
					t.Fatal("完成结果错误", result, err)
				}
				return err
			})
			expectedSequence := "begin,lock,upload,complete_upload"
			if scenario.readback != "" {
				expectedSequence += ",upload"
			}
			if scenario.wantError == nil {
				expectedSequence += ",commit"
			} else {
				expectedSequence += ",rollback"
			}
			if !errors.Is(err, scenario.wantError) || script.sequence() != expectedSequence {
				t.Fatal("完成错误或查询顺序不符", err, script.sequence())
			}
		})
	}
}
