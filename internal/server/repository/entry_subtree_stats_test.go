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

type entrySubtreeStatsRows struct {
	values []driver.Value
	read   bool
}

func (*entrySubtreeStatsRows) Columns() []string {
	return []string{"user_id", "entry_id", "node_count", "max_relative_depth", "invalid_parent"}
}
func (*entrySubtreeStatsRows) Close() error { return nil }
func (rows *entrySubtreeStatsRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestFindUserEntrySubtreeStatsResults(t *testing.T) {
	failure := errors.New("fixture subtree query failure")
	for _, testCase := range []struct {
		name   string
		column int
		value  driver.Value
		want   error
	}{
		{name: "ordinary", column: -1}, {name: "single file", column: -1}, {name: "empty directory", column: -1},
		{name: "at operation size", column: -1}, {name: "above operation size", column: -1}, {name: "maximum depth", column: -1}, {name: "maximum identifiers", column: -1},
		{name: "missing", column: -1, want: domain.ErrFileEntryNotFound},
		{name: "wrong owner", column: 0, value: int64(8), want: domain.ErrInvalidFileEntryData},
		{name: "wrong source", column: 1, value: int64(20), want: domain.ErrInvalidFileEntryData},
		{name: "zero source", column: 1, value: int64(0), want: domain.ErrInvalidFileEntryData},
		{name: "file parent", column: 4, value: int64(1), want: domain.ErrInvalidFileEntryData},
		{name: "too deep", column: 3, value: int64(33), want: domain.ErrInvalidFileEntryData},
		{name: "inconsistent depth", column: 2, value: int64(2), want: domain.ErrInvalidFileEntryData},
		{name: "no aggregate row", column: -1, want: domain.ErrInvalidFileEntryData},
		{name: "invalid count scan", column: 2, value: "private-count"},
		{name: "overflow source scan", column: 1, value: "18446744073709551616"},
		{name: "query error", column: -1, want: failure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			want := domain.EntrySubtreeStats{UserID: 7, EntryID: 19, NodeCount: 3, MaxRelativeDepth: 2}
			values := []driver.Value{int64(7), int64(19), int64(3), int64(2), int64(0)}
			switch testCase.name {
			case "single file", "empty directory":
				want.NodeCount, want.MaxRelativeDepth = 1, 0
				values[2], values[3] = int64(1), int64(0)
			case "at operation size":
				want.NodeCount, want.MaxRelativeDepth = 10000, 1
				values[2], values[3] = int64(10000), int64(1)
			case "above operation size":
				want.NodeCount, want.MaxRelativeDepth = 10001, 1
				values[2], values[3] = int64(10001), int64(1)
			case "maximum depth":
				want.NodeCount, want.MaxRelativeDepth = 33, 32
				values[2], values[3] = int64(33), int64(32)
			case "maximum identifiers":
				want.UserID, want.EntryID = ^uint64(0), ^uint64(0)
				values[0], values[1] = "18446744073709551615", "18446744073709551615"
			case "missing":
				values = []driver.Value{int64(0), int64(0), int64(0), int64(0), int64(0)}
			case "no aggregate row":
				values = nil
			}
			if testCase.column >= 0 {
				values[testCase.column] = testCase.value
			}
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				normalized := strings.Join(strings.Fields(statement), " ")
				for _, part := range []string{"WITH RECURSIVE entry_subtree AS", "WHERE user_id = ? AND id = ?", "child.user_id = parent.user_id AND child.parent_id = parent.id", "WHERE child.user_id = ? AND parent.relative_depth < ?", "COUNT(*) AS node_count", "MAX(relative_depth)"} {
					if !strings.Contains(normalized, part) {
						t.Error("递归查询结构缺失", part)
					}
				}
				if strings.Contains(normalized, "status") || len(arguments) != 4 || arguments[0].Value != want.UserID || arguments[1].Value != want.EntryID || arguments[2].Value != want.UserID || arguments[3].Value != int64(33) {
					t.Error("统计范围、生命周期或绑定参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("统计查询期限错误")
				}
				if testCase.name == "query error" {
					return nil, failure
				}
				return &entrySubtreeStatsRows{values: values}, nil
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			result, err := store.FindUserEntrySubtreeStats(context.Background(), want.UserID, want.EntryID)
			failed := testCase.want != nil || strings.Contains(testCase.name, "scan")
			if failed {
				if result != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) {
					t.Fatal("统计异常结果或错误链错误", err)
				}
			} else if err != nil || !reflect.DeepEqual(result, &want) {
				t.Fatal("子树统计结果错误", err, result)
			}
			if queries != 1 {
				t.Fatal("统计未使用单次查询")
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池", err)
			}
		})
	}
}

func TestFindUserEntrySubtreeStatsInputsAndCancellation(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero source", "cancelled input", "cancel during query", "short parent deadline", "independent results"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			userID, entryID := uint64(7), uint64(19)
			switch mode {
			case "nil context":
				callContext = nil
			case "zero user":
				userID = 0
			case "zero source":
				entryID = 0
			case "cancelled input":
				cancel()
			case "short parent deadline":
				short, release := context.WithTimeout(ctx, time.Second)
				defer release()
				callContext = short
			}
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				queries++
				if mode == "cancel during query" {
					cancel()
					return nil, errors.New("private-driver-cancel")
				}
				if mode == "short parent deadline" {
					deadline, _ := ctx.Deadline()
					if time.Until(deadline) > time.Second {
						t.Error("查询延长了调用方期限")
					}
				}
				return &entrySubtreeStatsRows{values: []driver.Value{int64(7), int64(19), int64(3), int64(2), int64(0)}}, nil
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			result, err := store.FindUserEntrySubtreeStats(callContext, userID, entryID)
			switch mode {
			case "short parent deadline", "independent results":
				if err != nil || result == nil {
					t.Fatal("合法调用错误", err)
				}
				if mode == "independent results" {
					second, err := store.FindUserEntrySubtreeStats(ctx, userID, entryID)
					if err != nil || second == result {
						t.Fatal("统计结果未独立持有", err)
					}
					result.NodeCount = 999
					if second.NodeCount != 3 {
						t.Fatal("修改一次结果影响其他结果")
					}
				}
			default:
				if result != nil || err == nil {
					t.Fatal("无效或取消调用未拒绝", err)
				}
				if strings.HasPrefix(mode, "cancel") && !errors.Is(err, context.Canceled) {
					t.Fatal("取消原因丢失", err)
				}
				if mode != "cancel during query" && queries != 0 {
					t.Fatal("非法输入执行了查询")
				}
			}
		})
	}
}
