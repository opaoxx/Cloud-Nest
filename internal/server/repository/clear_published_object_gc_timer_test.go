package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type publishedObjectReferenceRows struct{ *idempotencyRecordRows }

func (*publishedObjectReferenceRows) Columns() []string { return []string{"id", "file_size"} }

func TestClearPublishedObjectGCTimerResults(t *testing.T) {
	failure := errors.New("fixture timer update failure")
	for _, scenario := range []struct {
		name        string
		missing     bool
		alreadyDone bool
		rows        int64
		writeError  error
		wantError   error
	}{
		{name: "clear timer", rows: 1},
		{name: "already clear", alreadyDone: true},
		{name: "missing active file", missing: true, wantError: domain.ErrFileEntryNotFound},
		{name: "stale timer update", wantError: domain.ErrInvalidStoredObjectData},
		{name: "database update failure", writeError: failure, wantError: failure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			object, values := storedObjectReadFixture()
			oldStart, oldDeadline := object.UnreferencedAt, object.GCAfter
			if scenario.alreadyDone {
				values[12], values[13] = nil, nil
			}
			const entryID uint64 = 31
			script := &loginTransactionScript{status: "ACTIVE"}
			script.entryQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				expectedSQL := "SELECT id,file_size FROM file_entries WHERE user_id = ? AND id = ? AND object_id = ? AND entry_type = 'FILE' AND status = 'ACTIVE' LIMIT ? FOR UPDATE"
				if strings.ReplaceAll(statement, "`", "") != expectedSQL || len(arguments) != 4 ||
					arguments[0].Value != object.OwnerUserID || arguments[1].Value != entryID || arguments[2].Value != object.ID {
					t.Fatal("活动文件引用查询范围或行锁不符", statement, arguments)
				}
				var row []driver.Value
				if !scenario.missing {
					row = []driver.Value{entryID, object.FileSize}
				}
				return &publishedObjectReferenceRows{&idempotencyRecordRows{values: row}}, nil
			}
			script.objectQuery = func(_ context.Context, statement string, _ []driver.NamedValue) (driver.Rows, error) {
				if !strings.HasSuffix(statement, "FOR UPDATE") {
					t.Fatal("对象当前读缺少行锁", statement)
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.clearPublishedObjectTimer = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				if !strings.HasSuffix(statement, "WHERE owner_user_id = ? AND id = ? AND status = 'READY' AND unreferenced_at = ? AND gc_after = ?") ||
					len(arguments) != 4 || arguments[0].Value != object.OwnerUserID || arguments[1].Value != object.ID ||
					arguments[2].Value != *oldStart || arguments[3].Value != *oldDeadline {
					t.Fatal("清除计时条件或旧计时绑定不符", statement, arguments)
				}
				if scenario.rows == 1 {
					values[12], values[13] = nil, nil
				}
				return driver.RowsAffected(scenario.rows), scenario.writeError
			}
			store := loginTransactionTestStore(t, script)
			if result, err := store.ClearPublishedObjectGCTimer(t.Context(), object.ID, entryID); result != nil || err == nil {
				t.Fatal("事务外清除计时应被拒绝", err)
			}
			err := store.withActiveUserTransaction(t.Context(), object.OwnerUserID, func(ctx context.Context, txStore *Store) error {
				actual, err := txStore.ClearPublishedObjectGCTimer(ctx, object.ID, entryID)
				expected := object
				expected.UnreferencedAt, expected.GCAfter = nil, nil
				if scenario.wantError == nil && !reflect.DeepEqual(actual, &expected) || scenario.wantError != nil && actual != nil {
					t.Fatal("清除计时结果错误", actual, err)
				}
				return err
			})
			sequence := "begin,lock,entry"
			if !scenario.missing {
				sequence += ",object"
				if !scenario.alreadyDone {
					sequence += ",clear_object_timer"
				}
				if scenario.rows == 1 {
					sequence += ",object"
				}
			}
			if scenario.wantError == nil {
				sequence += ",commit"
			} else {
				sequence += ",rollback"
			}
			if !errors.Is(err, scenario.wantError) || script.sequence() != sequence {
				t.Fatal("清除计时错误或事务顺序错误", err, script.sequence())
			}
		})
	}
}
