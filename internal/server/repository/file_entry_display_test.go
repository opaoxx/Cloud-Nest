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

// fileEntryDisplayRows 提供九列展示数据，复用已有 SQL 测试连接器。
type fileEntryDisplayRows struct {
	values []driver.Value
	read   bool
}

func (*fileEntryDisplayRows) Columns() []string {
	return []string{"id", "user_id", "category", "extension", "mime_type", "source_device_id", "source_device_name", "color_key", "content_modified_at"}
}
func (*fileEntryDisplayRows) Close() error { return nil }
func (rows *fileEntryDisplayRows) Next(destination []driver.Value) error {
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestFindUserFileEntryDisplayResults(t *testing.T) {
	category, extension, mimeType := "DOCUMENT", "md", "text/markdown"
	sourceName, color := "  发布设备 🎮  ", "green"
	sourceID := uint64(13)
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	base := domain.FileEntryDisplay{
		ID: 19, UserID: 7, Category: &category, Extension: &extension, MIMEType: &mimeType,
		SourceDeviceID: &sourceID, SourceDeviceName: &sourceName, ContentModifiedAt: &stamp,
	}
	for _, testCase := range []struct {
		name     string
		column   int
		value    driver.Value
		queryErr error
		wantErr  error
		missing  bool
	}{
		{name: "full file display", column: -1},
		{name: "all nullable fields", column: -1},
		{name: "folder color", column: -1},
		{name: "empty strings preserved", column: -1},
		{name: "source name without id", column: -1},
		{name: "source id without name", column: -1},
		{name: "maximum unsigned values", column: -1},
		{name: "minimum database date", column: -1},
		{name: "maximum database date", column: -1},
		{name: "missing", missing: true, wantErr: domain.ErrFileEntryNotFound},
		{name: "wrong owner", column: 1, value: int64(8), wantErr: domain.ErrInvalidFileEntryData},
		{name: "wrong target", column: 0, value: int64(20), wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero result id", column: 0, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "zero source id", column: 5, value: int64(0), wantErr: domain.ErrInvalidFileEntryData},
		{name: "invalid id scan", column: 0, value: "bad-id"},
		{name: "negative source scan", column: 5, value: int64(-1)},
		{name: "overflow source scan", column: 5, value: "18446744073709551616"},
		{name: "invalid time scan", column: 8, value: "invalid-time"},
		{name: "query failure", column: -1, queryErr: errors.New("fixture display query failure")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备独立记录，核对九列投影、归属参数及读取期限。
			want := base
			values := []driver.Value{int64(19), int64(7), category, extension, mimeType, int64(13), sourceName, nil, stamp}
			switch testCase.name {
			case "all nullable fields":
				want = domain.FileEntryDisplay{ID: 19, UserID: 7}
				values = []driver.Value{int64(19), int64(7), nil, nil, nil, nil, nil, nil, nil}
			case "folder color":
				want = domain.FileEntryDisplay{ID: 19, UserID: 7, ColorKey: &color}
				values = []driver.Value{int64(19), int64(7), nil, nil, nil, nil, nil, color, nil}
			case "empty strings preserved":
				empty := ""
				want = domain.FileEntryDisplay{ID: 19, UserID: 7, Extension: &empty, MIMEType: &empty, SourceDeviceName: &empty, ColorKey: &empty}
				values = []driver.Value{int64(19), int64(7), nil, "", "", nil, "", "", nil}
			case "source name without id":
				want.SourceDeviceID, values[5] = nil, nil
			case "source id without name":
				want.SourceDeviceName, values[6] = nil, nil
			case "maximum unsigned values":
				maximum := ^uint64(0)
				want.ID, want.UserID, want.SourceDeviceID = maximum, maximum, &maximum
				for _, column := range []int{0, 1, 5} {
					values[column] = "18446744073709551615"
				}
			case "minimum database date":
				minimum := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				want.ContentModifiedAt, values[8] = &minimum, minimum
			case "maximum database date":
				maximum := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
				want.ContentModifiedAt, values[8] = &maximum, maximum
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
				expected := "SELECT id,user_id,category,extension,mime_type,source_device_id,source_device_name,color_key,content_modified_at FROM file_entries WHERE user_id = ? AND id = ? LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || len(arguments) != 3 || arguments[0].Value != want.UserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("展示查询投影、归属或绑定参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("展示查询期限错误")
				}
				return &fileEntryDisplayRows{values: values}, testCase.queryErr
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 核对全部可空字段和故障分类，共享池继续由调用方管理。
			result, err := store.FindUserFileEntryDisplay(context.Background(), want.UserID, want.ID)
			failed := testCase.wantErr != nil || testCase.queryErr != nil || strings.Contains(testCase.name, "scan")
			if failed {
				if result != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || (testCase.queryErr != nil && !errors.Is(err, testCase.queryErr)) {
					t.Fatal("展示查询未正确区分缺失或故障", err)
				}
			} else if err != nil || !reflect.DeepEqual(result, &want) {
				t.Fatal("展示字段原文、空值或完整整数未准确读回", err)
			}
			if queries != 1 || pool.PingContext(context.Background()) != nil {
				t.Fatal("展示查询次数或共享池生命周期错误")
			}
		})
	}
}

func TestFindUserFileEntryDisplayInput(t *testing.T) {
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
		result, err := store.FindUserFileEntryDisplay(testCase.ctx, testCase.userID, testCase.entryID)
		if result != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || connector.connections.Load() != 0 {
			t.Error("非法输入或预先取消仍访问数据库", err)
		}
	}
}

func TestFindUserFileEntryDisplayCancellation(t *testing.T) {
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
						return &fileEntryDisplayRows{}, nil
					}
					return &fileEntryDisplayRows{values: []driver.Value{int64(19), int64(7), nil, nil, nil, nil, nil, nil, nil}}, nil
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
			result, err := store.FindUserFileEntryDisplay(ctx, 7, 19)
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
