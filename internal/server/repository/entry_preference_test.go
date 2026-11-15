package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// entryPreferenceRows 提供关联查询的四列，复用已有 SQL 测试连接器。
type entryPreferenceRows struct {
	values []driver.Value
	read   bool
}

func (*entryPreferenceRows) Columns() []string {
	return []string{"user_id", "entry_id", "starred_at", "last_opened_at"}
}
func (*entryPreferenceRows) Close() error { return nil }
func (rows *entryPreferenceRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestFindUserEntryPreferenceResults(t *testing.T) {
	starredAt := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	openedAt := starredAt.Add(time.Second)
	base := domain.EntryPreference{UserID: 7, EntryID: 19, StarredAt: &starredAt, LastOpenedAt: &openedAt}
	for _, testCase := range []struct {
		name     string
		column   int
		value    driver.Value
		queryErr error
		wantErr  error
		missing  bool
	}{
		{name: "both timestamps", column: -1},
		{name: "starred only", column: -1},
		{name: "opened only", column: -1},
		{name: "no preference timestamps", column: -1},
		{name: "maximum unsigned ids", column: -1},
		{name: "database date boundaries", column: -1},
		{name: "missing target", missing: true, wantErr: domain.ErrFileEntryNotFound},
		{name: "wrong owner", column: 0, value: int64(8), wantErr: domain.ErrInvalidFileEntryData},
		{name: "wrong target", column: 1, value: int64(20), wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero owner in result", column: 0, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero target in result", column: 1, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "invalid user scan", column: 0, value: "bad-id"},
		{name: "invalid entry scan", column: 1, value: "bad-id"},
		{name: "negative entry scan", column: 1, value: int64(-1)},
		{name: "overflow user scan", column: 0, value: "18446744073709551616"},
		{name: "invalid starred time scan", column: 2, value: "invalid-time"},
		{name: "invalid opened time scan", column: 3, value: "invalid-time"},
		{name: "query failure", column: -1, queryErr: errors.New("fixture preference query failure")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备独立偏好，核对左连接、四列、双重归属和期限。
			want := base
			values := []driver.Value{int64(7), int64(19), starredAt, openedAt}
			switch testCase.name {
			case "starred only":
				want.LastOpenedAt, values[3] = nil, nil
			case "opened only":
				want.StarredAt, values[2] = nil, nil
			case "no preference timestamps":
				want.StarredAt, want.LastOpenedAt = nil, nil
				values[2], values[3] = nil, nil
			case "maximum unsigned ids":
				want.UserID, want.EntryID = ^uint64(0), ^uint64(0)
				values[0], values[1] = "18446744073709551615", "18446744073709551615"
			case "database date boundaries":
				minimum := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				maximum := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
				want.StarredAt, want.LastOpenedAt = &minimum, &maximum
				values[2], values[3] = minimum, maximum
			}
			if testCase.column >= 0 {
				values[testCase.column] = testCase.value
			}
			if testCase.missing {
				values = nil
			}
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				expected := "SELECT entries.user_id AS user_id,entries.id AS entry_id,preferences.starred_at AS starred_at,preferences.last_opened_at AS last_opened_at FROM file_entries AS entries LEFT JOIN user_entry_preferences AS preferences ON preferences.user_id = entries.user_id AND preferences.entry_id = entries.id WHERE entries.user_id = ? AND entries.id = ? LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || len(arguments) != 3 || arguments[0].Value != want.UserID || arguments[1].Value != want.EntryID || arguments[2].Value != int64(1) {
					t.Error("偏好投影、连接、归属或绑定参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("偏好查询期限错误")
				}
				return &entryPreferenceRows{values: values}, testCase.queryErr
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 核对时间空值、完整标识、错误原因和共享池生命周期。
			result, err := store.FindUserEntryPreference(context.Background(), want.UserID, want.EntryID)
			failed := testCase.wantErr != nil || testCase.queryErr != nil || strings.Contains(testCase.name, "scan")
			if failed {
				if result != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || (testCase.queryErr != nil && !errors.Is(err, testCase.queryErr)) {
					t.Fatal("偏好目标缺失或查询故障分类错误", err)
				}
			} else if err != nil || !reflect.DeepEqual(result, &want) {
				t.Fatal("偏好时间、空值或完整整数未准确读回", err)
			}
			if queries != 1 || pool.PingContext(context.Background()) != nil {
				t.Fatal("偏好查询次数或共享池生命周期错误")
			}
		})
	}
}

func TestFindUserEntryPreferenceInput(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer release()
	connector := &deviceConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		ctx             context.Context
		userID, entryID uint64
		wantErr         error
	}{
		{userID: 7, entryID: 19},
		{ctx: context.Background(), entryID: 19},
		{ctx: context.Background(), userID: 7},
		{ctx: cancelled, userID: 7, entryID: 19, wantErr: context.Canceled},
		{ctx: expired, userID: 7, entryID: 19, wantErr: context.DeadlineExceeded},
	} {
		result, err := store.FindUserEntryPreference(testCase.ctx, testCase.userID, testCase.entryID)
		if result != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || connector.connections.Load() != 0 {
			t.Error("非法输入或预先取消仍访问数据库", err)
		}
	}
}

func TestFindUserEntryPreferenceCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after result", "after missing", "after query failure"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "caller deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 30*time.Millisecond)
				defer release()
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.HasPrefix(phase, "after") {
					cancel()
					if phase == "after query failure" {
						return nil, errors.New("fixture cancelled query")
					}
					if phase == "after missing" {
						return &entryPreferenceRows{}, nil
					}
					return &entryPreferenceRows{values: []driver.Value{int64(7), int64(19), nil, nil}}, nil
				}
				<-queryContext.Done()
				return nil, queryContext.Err()
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			result, err := store.FindUserEntryPreference(ctx, 7, 19)
			wantErr := error(context.DeadlineExceeded)
			if strings.HasPrefix(phase, "after") {
				wantErr = context.Canceled
			}
			if result != nil || !errors.Is(err, wantErr) {
				t.Fatal("查询取消后返回了结果或丢失原因", err)
			}
		})
	}
}
