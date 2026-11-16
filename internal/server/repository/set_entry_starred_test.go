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

func TestSetUserEntryStarredResults(t *testing.T) {
	failure := errors.New("fixture starred failure")
	for _, testCase := range []struct {
		name     string
		starred  bool
		affected int64
		want     error
	}{
		{"first star", true, 1, nil}, {"existing star", true, 0, nil}, {"new star on preference", true, 2, nil}, {"found rows", true, 1, nil},
		{"unstar", false, 1, nil}, {"repeat unstar", false, 0, nil}, {"missing unstar", false, 0, nil},
		{"directory", true, 1, nil}, {"root", true, 1, nil}, {"maximum entry", true, 1, nil}, {"maximum revision", true, 1, nil}, {"date bounds", true, 0, nil},
		{"missing scope", true, 1, domain.ErrFileEntryNotFound}, {"scope fault", true, 1, failure}, {"wrong scope", true, 1, domain.ErrInvalidFileEntryData},
		{"missing locked target", true, 1, domain.ErrFileEntryNotFound}, {"lock fault", true, 1, failure}, {"wrong locked owner", true, 1, domain.ErrInvalidFileEntryData},
		{"zero revision", true, 1, domain.ErrInvalidFileEntryData}, {"zero entry time", true, 1, domain.ErrInvalidFileEntryData},
		{"trashed", true, 1, domain.ErrFileEntryNotFound}, {"purging", true, 1, domain.ErrFileEntryNotFound}, {"purged", true, 1, domain.ErrFileEntryNotFound},
		{"unknown type", true, 1, domain.ErrInvalidFileEntryData}, {"directory size", true, 1, domain.ErrInvalidFileEntryData},
		{"bad root", true, 1, domain.ErrInvalidFileEntryData}, {"zero parent", true, 1, domain.ErrInvalidFileEntryData}, {"self parent", true, 1, domain.ErrInvalidFileEntryData},
		{"invalid utf8", true, 1, domain.ErrInvalidFileEntryData}, {"empty name", true, 1, domain.ErrInvalidFileEntryData},
		{"write fault", true, 1, failure}, {"negative affected", true, -1, domain.ErrInvalidFileEntryData}, {"large star affected", true, 3, domain.ErrInvalidFileEntryData}, {"large unstar affected", false, 2, domain.ErrInvalidFileEntryData},
		{"missing star readback", true, 1, domain.ErrInvalidFileEntryData}, {"missing changed unstar", false, 1, domain.ErrInvalidFileEntryData}, {"readback fault", true, 1, failure},
		{"wrong preference owner", true, 1, domain.ErrInvalidFileEntryData}, {"wrong preference target", true, 1, domain.ErrInvalidFileEntryData},
		{"missing star time", true, 1, domain.ErrInvalidFileEntryData}, {"remaining star time", false, 1, domain.ErrInvalidFileEntryData},
		{"zero star time", true, 0, domain.ErrInvalidFileEntryData}, {"bad opened time", false, 0, domain.ErrInvalidFileEntryData},
		{"outer failure", true, 1, failure}, {"commit failure", true, 1, failure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, target, _, _ := renameEntryFixture()
			stamp, openedAt := target.CreatedAt.Add(time.Second), target.CreatedAt.Add(2*time.Second)
			var starTime *time.Time
			if testCase.starred {
				starTime = &stamp
			}
			var openTime *time.Time = &openedAt
			switch testCase.name {
			case "first star":
				openTime = nil
			case "directory":
				target.EntryType, target.FileSize = "DIR", 0
			case "root":
				target.ID, target.ParentID, target.Name, target.EntryType, target.FileSize = 11, nil, "", "DIR", 0
			case "maximum entry":
				target.ID = ^uint64(0)
			case "maximum revision":
				target.Revision = ^uint64(0)
			case "date bounds":
				stamp, openedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "zero revision":
				target.Revision = 0
			case "zero entry time":
				target.UpdatedAt = time.Time{}
			case "trashed":
				target.Status = "TRASHED"
			case "purging":
				target.Status = "PURGING"
			case "purged":
				target.Status = "PURGED"
			case "unknown type":
				target.EntryType = "LINK"
			case "directory size":
				target.EntryType = "DIR"
			case "bad root":
				target.ParentID = nil
			case "zero parent":
				parentID := uint64(0)
				target.ParentID = &parentID
			case "self parent":
				target.ParentID = &target.ID
			case "invalid utf8":
				target.Name = "name\xff"
			case "empty name":
				target.Name = ""
			case "missing star time":
				starTime = nil
			case "remaining star time":
				starTime = &stamp
			case "zero star time":
				stamp = time.Time{}
			case "bad opened time":
				openedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			script := &loginTransactionScript{}
			if testCase.name == "commit failure" {
				script.commitError = failure
			}
			store := revokeSessionTestStore(t, actor, script)
			reads, writes, preferenceReads := 0, 0, 0
			script.entryQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != target.ID {
					t.Fatal("收藏目录项范围绑定错误")
				}
				normalized := strings.ReplaceAll(statement, "`", "")
				if strings.HasPrefix(normalized, "SELECT id,user_id FROM ") {
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
				if !strings.HasSuffix(normalized, "FOR UPDATE") || reads != 2 {
					t.Fatal("目录项加锁顺序错误")
				}
				if testCase.name == "missing locked target" {
					return &fileEntryRows{}, nil
				}
				if testCase.name == "lock fault" {
					return nil, failure
				}
				locked := target
				if testCase.name == "wrong locked owner" {
					locked.UserID++
				}
				return &fileEntryRows{values: directoryEntryValues(locked)}, nil
			}
			script.setStarred = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				if reads != 2 || len(arguments) != 2 || arguments[0].Value != actor.UserID || arguments[1].Value != target.ID {
					t.Fatal("收藏写入顺序或归属错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("收藏写入期限错误")
				}
				wantSQL := "UPDATE user_entry_preferences SET starred_at = NULL, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND entry_id = ? AND starred_at IS NOT NULL"
				if testCase.starred {
					wantSQL = "INSERT INTO user_entry_preferences (user_id, entry_id, starred_at) VALUES (?, ?, UTC_TIMESTAMP(6)) ON DUPLICATE KEY UPDATE updated_at = IF(starred_at IS NULL, UTC_TIMESTAMP(6), updated_at), starred_at = COALESCE(starred_at, UTC_TIMESTAMP(6))"
				}
				if statement != wantSQL {
					t.Fatal("收藏 SQL 字段或条件错误")
				}
				if testCase.name == "write fault" {
					return nil, failure
				}
				return driver.RowsAffected(testCase.affected), nil
			}
			script.preferenceQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				preferenceReads++
				expected := "SELECT user_id,entry_id,starred_at,last_opened_at FROM user_entry_preferences WHERE user_id = ? AND entry_id = ? LIMIT ? FOR UPDATE"
				if strings.ReplaceAll(statement, "`", "") != expected || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != target.ID || writes != 1 {
					t.Fatal("偏好当前读取顺序、投影或范围错误")
				}
				if testCase.name == "readback fault" {
					return nil, failure
				}
				if testCase.name == "missing unstar" || testCase.name == "missing star readback" || testCase.name == "missing changed unstar" {
					return &entryPreferenceRows{}, nil
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
				if openTime != nil {
					openValue = *openTime
				}
				return &entryPreferenceRows{values: []driver.Value{strconv.FormatUint(owner, 10), strconv.FormatUint(entryID, 10), starValue, openValue}}, nil
			}
			var preference *domain.EntryPreference
			err := store.WithUserTransaction(context.Background(), actor, func(ctx context.Context, txStore *Store) error {
				var err error
				preference, err = txStore.SetUserEntryStarred(ctx, target.ID, testCase.starred)
				if err != nil {
					return err
				}
				if testCase.name == "outer failure" {
					return failure
				}
				return nil
			})
			if testCase.want != nil {
				if !errors.Is(err, testCase.want) {
					t.Fatal("收藏错误链不符", err)
				}
				if testCase.name != "outer failure" && testCase.name != "commit failure" && preference != nil {
					t.Fatal("方法错误返回了偏好")
				}
			} else {
				want := domain.EntryPreference{UserID: actor.UserID, EntryID: target.ID, StarredAt: starTime, LastOpenedAt: openTime}
				if testCase.name == "missing unstar" {
					want.StarredAt, want.LastOpenedAt = nil, nil
				}
				if err != nil || !reflect.DeepEqual(preference, &want) {
					t.Fatal("收藏结果不符", err)
				}
				if preference.StarredAt != nil && preference.StarredAt == starTime {
					t.Fatal("收藏时间共享输入指针")
				}
				if preference.LastOpenedAt != nil && preference.LastOpenedAt == openTime {
					t.Fatal("访问时间共享输入指针")
				}
				if writes != 1 || preferenceReads != 1 || !strings.HasSuffix(script.sequence(), "entry,entry,set_starred,preference,commit") {
					t.Fatal("收藏事务执行顺序错误", script.sequence())
				}
			}
		})
	}
}

func TestSetUserEntryStarredInputsAndCancellation(t *testing.T) {
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
			script.setStarred = func(context.Context, string, []driver.NamedValue) (driver.Result, error) {
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
				return &entryPreferenceRows{values: []driver.Value{int64(7), int64(23), target.CreatedAt, nil}}, nil
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
				preference, err := txStore.SetUserEntryStarred(callContext, entryID, true)
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
