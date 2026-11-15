package repository

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUserActiveFileEntryByNameResults(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	parentID := uint64(11)
	base := domain.FileEntry{
		ID: 19, UserID: 7, ParentID: &parentID, Name: "Café 🌱.MD", EntryType: "FILE",
		FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp.Add(time.Second), UpdatedAt: stamp.Add(2 * time.Second),
	}
	for _, testCase := range []struct {
		name     string
		column   int
		value    driver.Value
		queryErr error
		rowsErr  error
		wantErr  error
		missing  bool
	}{
		{name: "file and unicode", column: -1},
		{name: "directory", column: -1},
		{name: "maximum unsigned values", column: -1},
		{name: "single byte key", column: -1},
		{name: "maximum key capacity", column: -1},
		{name: "binary key with zero and invalid utf8", column: -1},
		{name: "sql punctuation key", column: -1},
		{name: "missing", column: -1, missing: true, wantErr: domain.ErrFileEntryNotFound},
		{name: "wrong owner", column: 1, value: int64(8), wantErr: domain.ErrInvalidFileEntryData},
		{name: "wrong parent", column: 2, value: int64(12), wantErr: domain.ErrInvalidFileEntryData},
		{name: "null parent", column: 2, value: nil, wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero id", column: 0, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero revision", column: 6, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "trashed result", column: 7, value: "TRASHED", wantErr: domain.ErrInvalidFileEntryData},
		{name: "purging result", column: 7, value: "PURGING", wantErr: domain.ErrInvalidFileEntryData},
		{name: "purged result", column: 7, value: "PURGED", wantErr: domain.ErrInvalidFileEntryData},
		{name: "invalid id scan", column: 0, value: "bad-id"},
		{name: "negative size scan", column: 5, value: int64(-1)},
		{name: "overflow revision scan", column: 6, value: "18446744073709551616"},
		{name: "invalid time scan", column: 8, value: "bad-time"},
		{name: "query failure", column: -1, queryErr: errors.New("fixture name query failure")},
		{name: "row iteration failure", column: -1, missing: true, rowsErr: errors.New("fixture name row failure")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 核对十一列投影、四项绑定条件和最多五秒的期限。
			want := base
			key := []byte("café 🌱.md")
			switch testCase.name {
			case "directory":
				want.EntryType, want.FileSize = "DIR", 0
			case "maximum unsigned values":
				maximum := ^uint64(0)
				want.ID, want.UserID, want.ParentID, want.FileSize, want.Revision = maximum, maximum, &maximum, maximum, maximum
			case "single byte key":
				key = []byte("a")
			case "maximum key capacity":
				key = bytes.Repeat([]byte{0xff}, 768)
			case "binary key with zero and invalid utf8":
				key = []byte{'a', 0, 0xff, ' '}
			case "sql punctuation key":
				key = []byte("x' OR 1=1 -- %_")
			}
			values := directoryEntryValues(want)
			if testCase.column >= 0 {
				values[testCase.column] = testCase.value
			}
			var records [][]driver.Value
			if !testCase.missing {
				records = append(records, values)
			}
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				expected := "SELECT id,user_id,parent_id,name,entry_type,file_size,revision,status,created_at,modified_at,updated_at FROM file_entries WHERE user_id = ? AND parent_id = ? AND active_name_key = ? AND status = ? LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || len(arguments) != 5 || arguments[0].Value != want.UserID || arguments[1].Value != *want.ParentID || !bytes.Equal(arguments[2].Value.([]byte), key) || arguments[3].Value != "ACTIVE" || arguments[4].Value != int64(1) {
					t.Error("活动同名查询投影、范围、二进制键或绑定参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("活动同名查询期限错误")
				}
				return &directoryEntryRows{records: records, terminalError: testCase.rowsErr}, testCase.queryErr
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 区分缺失、数据异常、扫描故障和正常结果。
			result, err := store.FindUserActiveFileEntryByName(context.Background(), want.UserID, *want.ParentID, key)
			failed := testCase.wantErr != nil || testCase.queryErr != nil || testCase.rowsErr != nil || strings.Contains(testCase.name, "scan")
			if failed {
				if result != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || (testCase.queryErr != nil && !errors.Is(err, testCase.queryErr)) || (testCase.rowsErr != nil && !errors.Is(err, testCase.rowsErr)) {
					t.Fatal("活动同名查询未保留失败类别和空结果", err)
				}
			} else if err != nil || !reflect.DeepEqual(result, &want) {
				t.Fatal("活动同名项基础字段错误", err)
			}
			if queries != 1 {
				t.Fatal("活动同名查询未恰好执行一次")
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("活动同名查询关闭了共享连接池", err)
			}
		})
	}
}

func TestFindUserActiveFileEntryByNameInput(t *testing.T) {
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
		ctx              context.Context
		userID, parentID uint64
		key              []byte
		wantErr          error
	}{
		{userID: 7, parentID: 11, key: []byte("a")},
		{ctx: context.Background(), parentID: 11, key: []byte("a")},
		{ctx: context.Background(), userID: 7, key: []byte("a")},
		{ctx: context.Background(), userID: 7, parentID: 11},
		{ctx: context.Background(), userID: 7, parentID: 11, key: []byte{}},
		{ctx: context.Background(), userID: 7, parentID: 11, key: bytes.Repeat([]byte("a"), 769)},
		{ctx: cancelled, userID: 7, parentID: 11, key: []byte("a"), wantErr: context.Canceled},
		{ctx: expired, userID: 7, parentID: 11, key: []byte("a"), wantErr: context.DeadlineExceeded},
	} {
		result, err := store.FindUserActiveFileEntryByName(testCase.ctx, testCase.userID, testCase.parentID, testCase.key)
		if result != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || connector.connections.Load() != 0 {
			t.Error("非法输入或预先取消仍访问数据库", err)
		}
	}
}

func TestFindUserActiveFileEntryByNameCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after result", "after missing", "after failure"} {
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
					if phase == "after failure" {
						return nil, errors.New("fixture cancelled name query")
					}
					if phase == "after missing" {
						return &fileEntryRows{}, nil
					}
					stamp := time.Now().UTC()
					return &fileEntryRows{values: []driver.Value{int64(19), int64(7), int64(11), "目录", "DIR", int64(0), int64(1), "ACTIVE", stamp, stamp, stamp}}, nil
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
			result, err := store.FindUserActiveFileEntryByName(ctx, 7, 11, []byte("目录"))
			want := error(context.DeadlineExceeded)
			if strings.HasPrefix(phase, "after") {
				want = context.Canceled
			}
			if result != nil || !errors.Is(err, want) {
				t.Fatal("取消后返回同名项或丢失取消原因", err)
			}
		})
	}
}

func TestFindUserActiveFileEntryByNameKeyCopy(t *testing.T) {
	key := []byte("readme.md")
	connector := &deviceConnector{accountConnector: accountConnector{query: func(_ context.Context, _ string, arguments []driver.NamedValue) (driver.Rows, error) {
		arguments[2].Value.([]byte)[0] = '!'
		return &fileEntryRows{}, nil
	}}}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := store.FindUserActiveFileEntryByName(context.Background(), 7, 11, key); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) || string(key) != "readme.md" {
		t.Fatal("驱动获得了调用方可变名称键的原始切片", err)
	}
}
