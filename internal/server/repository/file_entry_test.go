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

// fileEntryRows 提供目录项查询的十一列，复用已有 SQL 测试连接器。
type fileEntryRows struct {
	values []driver.Value
	read   bool
}

func (*fileEntryRows) Columns() []string {
	return []string{"id", "user_id", "parent_id", "name", "entry_type", "file_size", "revision", "status", "created_at", "modified_at", "updated_at"}
}
func (*fileEntryRows) Close() error { return nil }
func (rows *fileEntryRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestFindUserFileEntryResults(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	parentID := uint64(11)
	base := domain.FileEntry{
		ID: 19, UserID: 7, ParentID: &parentID, Name: "学习笔记 🌱.md", EntryType: "FILE",
		FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp.Add(time.Second), UpdatedAt: stamp.Add(2 * time.Second),
	}
	for _, testCase := range []struct {
		name     string
		column   int
		value    driver.Value
		queryErr error
		wantErr  error
		missing  bool
	}{
		{name: "file and unicode", column: -1},
		{name: "directory", column: -1},
		{name: "root with null parent", column: -1},
		{name: "trashed preserved", column: 7, value: "TRASHED"},
		{name: "purging preserved", column: 7, value: "PURGING"},
		{name: "purged size preserved", column: 7, value: "PURGED"},
		{name: "maximum unsigned values", column: -1},
		{name: "missing", missing: true, wantErr: domain.ErrFileEntryNotFound},
		{name: "wrong owner", column: 1, value: int64(8), wantErr: domain.ErrInvalidFileEntryData},
		{name: "wrong target", column: 0, value: int64(20), wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero target in result", column: 0, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero parent", column: 2, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero revision", column: 6, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "invalid id scan", column: 0, value: "bad-id"},
		{name: "negative parent scan", column: 2, value: int64(-1)},
		{name: "negative size scan", column: 5, value: int64(-1)},
		{name: "overflow revision scan", column: 6, value: "18446744073709551616"},
		{name: "query failure", column: -1, queryErr: errors.New("fixture query failure")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备独立记录，核对投影、归属参数及读取期限。
			want := base
			values := []driver.Value{int64(19), int64(7), int64(11), base.Name, "FILE", int64(2048), int64(3), "ACTIVE", base.CreatedAt, base.ModifiedAt, base.UpdatedAt}
			switch testCase.name {
			case "directory":
				want.Name, want.EntryType, want.FileSize = "学习目录", "DIR", 0
				values[3], values[4], values[5] = want.Name, want.EntryType, int64(0)
			case "root with null parent":
				want.ParentID, want.Name, want.EntryType, want.FileSize = nil, "", "DIR", 0
				values[2], values[3], values[4], values[5] = nil, "", "DIR", int64(0)
			case "maximum unsigned values":
				maximum := ^uint64(0)
				want.ID, want.UserID, want.ParentID, want.FileSize, want.Revision = maximum, maximum, &maximum, maximum, maximum
				for _, column := range []int{0, 1, 2, 5, 6} {
					values[column] = "18446744073709551615"
				}
			}
			if testCase.column >= 0 {
				values[testCase.column] = testCase.value
				if testCase.column == 7 {
					want.Status = testCase.value.(string)
				}
			}
			if testCase.missing {
				values = nil
			}
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				expected := "SELECT id,user_id,parent_id,name,entry_type,file_size,revision,status,created_at,modified_at,updated_at FROM file_entries WHERE user_id = ? AND id = ? LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || len(arguments) != 3 || arguments[0].Value != want.UserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("查询投影、归属条件或绑定参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("目录项查询期限错误")
				}
				return &fileEntryRows{values: values}, testCase.queryErr
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 核对正常、缺失和故障结果，查询后共享池仍由调用方管理。
			result, err := store.FindUserFileEntry(context.Background(), want.UserID, want.ID)
			failed := testCase.wantErr != nil || testCase.queryErr != nil || strings.Contains(testCase.name, "scan")
			if failed {
				if result != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || (testCase.queryErr != nil && !errors.Is(err, testCase.queryErr)) {
					t.Fatal("目录项缺失或异常结果错误", err)
				}
			} else if err != nil || !reflect.DeepEqual(result, &want) {
				t.Fatal("目录项基础字段未完整读回", err)
			}
			if queries != 1 {
				t.Fatal("目录项查询未恰好执行一次")
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池")
			}
		})
	}
}

func TestFindUserFileEntryInput(t *testing.T) {
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
		result, err := store.FindUserFileEntry(testCase.ctx, testCase.userID, testCase.entryID)
		if result != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || connector.connections.Load() != 0 {
			t.Error("非法输入或预先取消仍访问数据库", err)
		}
	}
}

func TestFindUserFileEntryCancellation(t *testing.T) {
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
						return &fileEntryRows{}, nil
					}
					stamp := time.Now().UTC()
					return &fileEntryRows{values: []driver.Value{int64(19), int64(7), nil, "", "DIR", int64(0), int64(1), "ACTIVE", stamp, stamp, stamp}}, nil
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
			result, err := store.FindUserFileEntry(ctx, 7, 19)
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
