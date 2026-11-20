package repository

import (
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

type fileEntryContentRows struct{ *idempotencyRecordRows }

func (*fileEntryContentRows) Columns() []string {
	return []string{"id", "user_id", "object_id", "mime_type"}
}

func TestFindUserFileEntryContentResults(t *testing.T) {
	// 1. 为四列查询提供合法引用、空值和各类故障。
	const userID, entryID = uint64(7), uint64(19)
	const query = "SELECT id,user_id,object_id,mime_type FROM file_entries WHERE user_id = ? AND id = ? LIMIT ?"
	databaseFailure := errors.New("fixture content read failure")
	invalidData := domain.ErrInvalidFileEntryData
	for _, scenario := range []struct {
		name     string
		row      []driver.Value
		queryErr error
		wantErr  error
		want     *domain.FileEntryContent
	}{
		{name: "reference with MIME", row: []driver.Value{int64(19), int64(7), int64(41), "image/png"},
			want: &domain.FileEntryContent{ID: entryID, UserID: userID, ObjectID: new(uint64(41)), MIMEType: new("image/png")}},
		{name: "reference without MIME", row: []driver.Value{int64(19), int64(7), "18446744073709551615", nil},
			want: &domain.FileEntryContent{ID: entryID, UserID: userID, ObjectID: new(^uint64(0))}},
		{name: "empty MIME preserved", row: []driver.Value{int64(19), int64(7), int64(41), ""},
			want: &domain.FileEntryContent{ID: entryID, UserID: userID, ObjectID: new(uint64(41)), MIMEType: new("")}},
		{name: "nullable reference", row: []driver.Value{int64(19), int64(7), nil, nil},
			want: &domain.FileEntryContent{ID: entryID, UserID: userID}},
		{name: "missing", wantErr: domain.ErrFileEntryNotFound},
		{name: "wrong entry ID", row: []driver.Value{int64(20), int64(7), int64(41), nil}, wantErr: invalidData},
		{name: "wrong user ID", row: []driver.Value{int64(19), int64(8), int64(41), nil}, wantErr: invalidData},
		{name: "zero object ID", row: []driver.Value{int64(19), int64(7), int64(0), nil}, wantErr: invalidData},
		{name: "database failure", queryErr: databaseFailure, wantErr: databaseFailure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 2. 核对查询列、归属参数和期限，模拟数据库返回。
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, args []driver.NamedValue) (driver.Rows, error) {
				queries++
				if strings.ReplaceAll(statement, "`", "") != query || len(args) != 3 ||
					args[0].Value != userID || args[1].Value != entryID || args[2].Value != int64(1) {
					t.Error("内容引用查询投影或归属参数错误", statement, args)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("内容引用查询期限错误")
				}
				if scenario.queryErr != nil {
					return nil, scenario.queryErr
				}
				return &fileEntryContentRows{&idempotencyRecordRows{values: scenario.row}}, nil
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 3. 核对数据和错误类别，确保请求只读一次。
			content, err := store.FindUserFileEntryContent(t.Context(), userID, entryID)
			if !errors.Is(err, scenario.wantErr) || !reflect.DeepEqual(content, scenario.want) || queries != 1 {
				t.Fatal("内容引用读取错误", content, err, queries)
			}
		})
	}
}

func TestFindUserFileEntryContentInput(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, release := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer release()
	connector := &deviceConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		ctx             context.Context
		userID, entryID uint64
		wantErr         error
	}{
		{userID: 7, entryID: 19},
		{ctx: t.Context(), entryID: 19},
		{ctx: t.Context(), userID: 7},
		{ctx: cancelled, userID: 7, entryID: 19, wantErr: context.Canceled},
		{ctx: expired, userID: 7, entryID: 19, wantErr: context.DeadlineExceeded},
	} {
		result, err := store.FindUserFileEntryContent(invalid.ctx, invalid.userID, invalid.entryID)
		if result != nil || err == nil || (invalid.wantErr != nil && !errors.Is(err, invalid.wantErr)) ||
			connector.connections.Load() != 0 {
			t.Fatal("非法输入或取消请求仍访问数据库", err)
		}
	}
}

func TestFindUserFileEntryContentCancellation(t *testing.T) {
	// 1. 核对较短的调用方期限，并在查询开始后取消请求。
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
		queryDeadline, _ := queryContext.Deadline()
		requestDeadline, _ := ctx.Deadline()
		if !queryDeadline.Equal(requestDeadline) {
			t.Error("查询延长了调用方的期限")
		}
		cancel()
		return nil, errors.New("fixture interrupted query")
	}}}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	// 2. 查询被中止时优先保留调用方取消错误。
	result, err := store.FindUserFileEntryContent(ctx, 7, 19)
	if result != nil || !errors.Is(err, context.Canceled) || connector.connections.Load() != 1 {
		t.Fatal("查询取消结果或连接数错误", err)
	}
}
