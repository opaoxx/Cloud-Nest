package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRecordUserEntryAccessResults(t *testing.T) {
	failure := errors.New("fixture access failure")
	for _, testCase := range []struct {
		name     string
		affected int64
		want     error
		writes   int
	}{
		{"first access", 1, nil, 1}, {"repeat access", 0, nil, 1}, {"aged access", 2, nil, 1}, {"found rows", 1, nil, 1},
		{"empty file", 1, nil, 1}, {"maximum entry", 1, nil, 1}, {"maximum revision", 0, nil, 1}, {"date bounds", 0, nil, 1},
		{"missing scope", 0, domain.ErrFileEntryNotFound, 0}, {"scope fault", 0, failure, 0}, {"wrong scope", 0, domain.ErrInvalidFileEntryData, 0},
		{"missing target", 0, domain.ErrFileEntryNotFound, 0}, {"lock fault", 0, failure, 0}, {"wrong owner", 0, domain.ErrInvalidFileEntryData, 0},
		{"zero revision", 0, domain.ErrInvalidFileEntryData, 0}, {"zero time", 0, domain.ErrInvalidFileEntryData, 0},
		{"trashed", 0, domain.ErrFileEntryNotFound, 0}, {"purging", 0, domain.ErrFileEntryNotFound, 0}, {"purged", 0, domain.ErrFileEntryNotFound, 0},
		{"directory", 0, domain.ErrFileEntryNotFile, 0}, {"root", 0, domain.ErrFileEntryNotFile, 0}, {"bad directory", 0, domain.ErrInvalidFileEntryData, 0},
		{"unknown type", 0, domain.ErrInvalidFileEntryData, 0}, {"nil parent", 0, domain.ErrInvalidFileEntryData, 0}, {"zero parent", 0, domain.ErrInvalidFileEntryData, 0}, {"self parent", 0, domain.ErrInvalidFileEntryData, 0},
		{"empty name", 0, domain.ErrInvalidFileEntryData, 0}, {"invalid utf8", 0, domain.ErrInvalidFileEntryData, 0},
		{"write fault", 1, failure, 1}, {"negative affected", -1, domain.ErrInvalidFileEntryData, 1}, {"large affected", 3, domain.ErrInvalidFileEntryData, 1},
		{"missing preference", 1, domain.ErrInvalidFileEntryData, 1}, {"readback fault", 1, failure, 1},
		{"wrong preference owner", 1, domain.ErrInvalidFileEntryData, 1}, {"wrong preference target", 1, domain.ErrInvalidFileEntryData, 1},
		{"missing opened", 1, domain.ErrInvalidFileEntryData, 1}, {"zero opened", 0, domain.ErrInvalidFileEntryData, 1}, {"zero starred", 0, domain.ErrInvalidFileEntryData, 1},
		{"early opened", 0, domain.ErrInvalidFileEntryData, 1}, {"late starred", 0, domain.ErrInvalidFileEntryData, 1},
		{"outer failure", 1, failure, 1}, {"commit failure", 1, failure, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, target, _, _ := renameEntryFixture()
			starredAt, openedAt := target.CreatedAt, target.CreatedAt.Add(time.Second)
			var starTime *time.Time = &starredAt
			switch testCase.name {
			case "first access":
				starTime = nil
			case "empty file":
				target.FileSize = 0
			case "maximum entry":
				target.ID = ^uint64(0)
			case "maximum revision":
				target.Revision = ^uint64(0)
			case "date bounds":
				starredAt, openedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "zero revision":
				target.Revision = 0
			case "zero time":
				target.UpdatedAt = time.Time{}
			case "trashed", "purging", "purged":
				target.Status = strings.ToUpper(testCase.name)
			case "directory", "root":
				target.EntryType, target.FileSize = "DIR", 0
				if testCase.name == "root" {
					target.ParentID, target.Name = nil, ""
				}
			case "bad directory":
				target.EntryType = "DIR"
			case "unknown type":
				target.EntryType = "LINK"
			case "nil parent":
				target.ParentID = nil
			case "zero parent":
				parentID := uint64(0)
				target.ParentID = &parentID
			case "self parent":
				target.ParentID = &target.ID
			case "empty name":
				target.Name = ""
			case "invalid utf8":
				target.Name = "name\xff"
			case "zero opened":
				openedAt = time.Time{}
			case "zero starred":
				starredAt = time.Time{}
			case "early opened":
				openedAt = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)
			case "late starred":
				starredAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			script := &loginTransactionScript{}
			if testCase.name == "commit failure" {
				script.commitError = failure
			}
			store := revokeSessionTestStore(t, actor, script)
			reads, writes := 0, 0
			script.entryQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != target.ID {
					t.Fatal("访问目标查询归属错误")
				}
				if strings.HasPrefix(strings.ReplaceAll(statement, "`", ""), "SELECT id,user_id FROM ") {
					if testCase.name == "missing scope" {
						return &renameDeviceScopeRows{}, nil
					}
					if testCase.name == "scope fault" {
						return nil, failure
					}
					owner := actor.UserID
					if testCase.name == "wrong scope" {
						owner++
					}
					return &renameDeviceScopeRows{values: []driver.Value{strconv.FormatUint(target.ID, 10), strconv.FormatUint(owner, 10)}}, nil
				}
				if reads != 2 || !strings.HasSuffix(statement, "FOR UPDATE") {
					t.Fatal("目标锁定顺序错误")
				}
				if testCase.name == "missing target" {
					return &fileEntryRows{}, nil
				}
				if testCase.name == "lock fault" {
					return nil, failure
				}
				locked := target
				if testCase.name == "wrong owner" {
					locked.UserID++
				}
				return &fileEntryRows{values: directoryEntryValues(locked)}, nil
			}
			script.recordAccess = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				wantSQL := "INSERT INTO user_entry_preferences (user_id, entry_id, last_opened_at) VALUES (?, ?, UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE updated_at = IF(last_opened_at IS NULL OR last_opened_at <= UTC_TIMESTAMP(6) - INTERVAL 60 SECOND, UTC_TIMESTAMP(6), updated_at), last_opened_at = IF(last_opened_at IS NULL OR last_opened_at <= UTC_TIMESTAMP(6) - INTERVAL 60 SECOND, UTC_TIMESTAMP(6), last_opened_at)"
				if statement != wantSQL || reads != 2 || len(arguments) != 2 || arguments[0].Value != actor.UserID || arguments[1].Value != target.ID {
					t.Fatal("访问写入 SQL、参数或顺序错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("访问写入期限错误")
				}
				if testCase.name == "write fault" {
					return nil, failure
				}
				return driver.RowsAffected(testCase.affected), nil
			}
			script.preferenceQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if strings.ReplaceAll(statement, "`", "") != "SELECT user_id,entry_id,starred_at,last_opened_at FROM user_entry_preferences WHERE user_id = ? AND entry_id = ? LIMIT ? FOR UPDATE" || writes != 1 || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != target.ID {
					t.Fatal("偏好当前读取错误")
				}
				if testCase.name == "missing preference" {
					return &entryPreferenceRows{}, nil
				}
				if testCase.name == "readback fault" {
					return nil, failure
				}
				owner, entryID := actor.UserID, target.ID
				if testCase.name == "wrong preference owner" {
					owner++
				}
				if testCase.name == "wrong preference target" {
					entryID++
				}
				var starValue, openValue driver.Value
				if starTime != nil {
					starValue = *starTime
				}
				if testCase.name != "missing opened" {
					openValue = openedAt
				}
				return &entryPreferenceRows{values: []driver.Value{strconv.FormatUint(owner, 10), strconv.FormatUint(entryID, 10), starValue, openValue}}, nil
			}
			var preference *domain.EntryPreference
			err := store.WithUserTransaction(context.Background(), actor, func(ctx context.Context, txStore *Store) error {
				var err error
				preference, err = txStore.RecordUserEntryAccess(ctx, target.ID)
				if err == nil && testCase.name == "outer failure" {
					return failure
				}
				return err
			})
			if writes != testCase.writes {
				t.Fatal("写入次数错误", writes)
			}
			if testCase.want != nil {
				if !errors.Is(err, testCase.want) || (preference != nil && testCase.name != "outer failure" && testCase.name != "commit failure") {
					t.Fatal("访问错误链或空结果错误", err)
				}
				return
			}
			want := &domain.EntryPreference{UserID: actor.UserID, EntryID: target.ID, StarredAt: starTime, LastOpenedAt: &openedAt}
			if err != nil || !reflect.DeepEqual(preference, want) || !strings.HasSuffix(script.sequence(), "entry,entry,record_access,preference,commit") {
				t.Fatal("访问结果或事务顺序错误", err, script.sequence())
			}
			if preference.LastOpenedAt == &openedAt || (starTime != nil && preference.StarredAt == starTime) {
				t.Fatal("时间副本错误")
			}
		})
	}
}

func TestRecordUserEntryAccessInputsAndCancellation(t *testing.T) {
	for _, mode := range []string{"plain store", "nontransaction store", "nil context", "zero entry", "missing actor", "zero bound user", "wrong bound user", "zero device", "zero session", "canceled input", "expired input", "caller deadline", "scope cancel", "lock cancel", "write cancel", "readback cancel"} {
		t.Run(mode, func(t *testing.T) {
			actor, target, _, _ := renameEntryFixture()
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			entryID := target.ID
			if mode == "nil context" {
				callContext = nil
			}
			if mode == "zero entry" {
				entryID = 0
			}
			if mode == "canceled input" {
				cancel()
			}
			if mode == "expired input" || mode == "caller deadline" {
				duration := -time.Second
				if mode == "caller deadline" {
					duration = 5 * time.Millisecond
				}
				var release context.CancelFunc
				callContext, release = context.WithTimeout(ctx, duration)
				defer release()
			}
			entryReads, writes := 0, 0
			script.entryQuery = func(queryContext context.Context, statement string, _ []driver.NamedValue) (driver.Rows, error) {
				entryReads++
				if mode == "caller deadline" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if strings.HasPrefix(strings.ReplaceAll(statement, "`", ""), "SELECT id,user_id FROM ") {
					if mode == "scope cancel" {
						cancel()
					}
					return &renameDeviceScopeRows{values: []driver.Value{int64(23), int64(7)}}, nil
				}
				if mode == "lock cancel" {
					cancel()
				}
				return &fileEntryRows{values: directoryEntryValues(target)}, nil
			}
			script.recordAccess = func(context.Context, string, []driver.NamedValue) (driver.Result, error) {
				writes++
				if mode == "write cancel" {
					cancel()
				}
				return driver.RowsAffected(1), nil
			}
			script.preferenceQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				if mode == "readback cancel" {
					cancel()
				}
				return &entryPreferenceRows{values: []driver.Value{int64(7), int64(23), nil, target.CreatedAt}}, nil
			}
			invoke := func(txStore *Store) error {
				switch mode {
				case "missing actor":
					txStore.authenticatedActor = nil
				case "zero bound user":
					txStore.loginUserID = 0
				case "wrong bound user":
					txStore.loginUserID++
				case "zero device":
					txStore.authenticatedActor.DeviceID = 0
				case "zero session":
					txStore.authenticatedActor.SessionID = 0
				}
				preference, err := txStore.RecordUserEntryAccess(callContext, entryID)
				if preference != nil || err == nil {
					t.Fatal("非法调用或取消未拒绝", err)
				}
				if strings.Contains(mode, "cancel") && !errors.Is(err, context.Canceled) {
					t.Fatal("取消原因丢失", err)
				}
				if (mode == "expired input" || mode == "caller deadline") && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("期限原因丢失", err)
				}
				return err
			}
			if mode == "plain store" {
				_ = invoke(store)
			} else if mode == "nontransaction store" {
				store.authenticatedActor, store.loginUserID = &actor, actor.UserID
				_ = invoke(store)
			} else {
				_ = store.WithUserTransaction(context.Background(), actor, func(_ context.Context, txStore *Store) error { return invoke(txStore) })
			}
			if !strings.HasSuffix(mode, "cancel") && mode != "caller deadline" && (entryReads != 0 || writes != 0) {
				t.Fatal("非法输入访问了目录项")
			}
		})
	}
}
