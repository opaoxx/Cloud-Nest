package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type directoryEntryRows struct {
	records       [][]driver.Value
	position      int
	terminalError error
}

func (*directoryEntryRows) Columns() []string { return (&fileEntryRows{}).Columns() }
func (*directoryEntryRows) Close() error      { return nil }
func (rows *directoryEntryRows) Next(destination []driver.Value) error {
	if rows.position == len(rows.records) {
		if rows.terminalError != nil {
			return rows.terminalError
		}
		return io.EOF
	}
	copy(destination, rows.records[rows.position])
	rows.position++
	return nil
}

func directoryEntryValues(entry domain.FileEntry) []driver.Value {
	var parentID driver.Value
	if entry.ParentID != nil {
		parentID = strconv.FormatUint(*entry.ParentID, 10)
	}
	return []driver.Value{
		strconv.FormatUint(entry.ID, 10), strconv.FormatUint(entry.UserID, 10), parentID,
		entry.Name, entry.EntryType, strconv.FormatUint(entry.FileSize, 10), strconv.FormatUint(entry.Revision, 10),
		entry.Status, entry.CreatedAt, entry.ModifiedAt, entry.UpdatedAt,
	}
}

func TestListUserDirectoryEntriesResults(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	parentID := uint64(11)
	base := domain.FileEntry{ID: 19, UserID: 7, ParentID: &parentID, Name: "学习笔记 🌱.md", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp.Add(-time.Hour), ModifiedAt: stamp, UpdatedAt: stamp}
	sameTime, olderTime := base, base
	sameTime.ID, olderTime.ID, olderTime.ModifiedAt = 18, 99, stamp.Add(-time.Second)
	directory := base
	directory.EntryType, directory.FileSize, directory.Name = "DIR", 0, "资料"
	position := &domain.FileEntryPosition{ModifiedAt: stamp, ID: 20}
	offsetPosition := &domain.FileEntryPosition{ModifiedAt: stamp.In(time.FixedZone("fixture", 8*3600)), ID: 20}
	maximum := ^uint64(0)
	large := base
	large.ID, large.UserID, large.ParentID, large.FileSize, large.Revision = maximum, maximum, &maximum, maximum, maximum
	var fullEntries []domain.FileEntry
	for index := 0; index < 101; index++ {
		entry := base
		entry.ID = uint64(201 - index)
		fullEntries = append(fullEntries, entry)
	}
	queryFailure, rowFailure := errors.New("fixture query"), errors.New("fixture partial rows")
	for _, testCase := range []struct {
		name                            string
		entries                         []domain.FileEntry
		before                          *domain.FileEntryPosition
		pageSize                        int
		userID, parentID                uint64
		change                          func([][]driver.Value)
		queryError, rowError, wantError error
		want                            *domain.FileEntryPage
	}{
		{name: "empty page", want: &domain.FileEntryPage{Entries: []domain.FileEntry{}}},
		{name: "single file", entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "directory", entries: []domain.FileEntry{directory}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{directory}}},
		{name: "same timestamp ordered by id", entries: []domain.FileEntry{base, sameTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, sameTime}}},
		{name: "timestamp before id", entries: []domain.FileEntry{base, olderTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, olderTime}}},
		{name: "extra row", entries: []domain.FileEntry{base, sameTime, olderTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, sameTime}, HasMore: true}},
		{name: "seek same timestamp", before: position, entries: []domain.FileEntry{base, sameTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, sameTime}}},
		{name: "seek older timestamp larger id", before: position, entries: []domain.FileEntry{olderTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{olderTime}}},
		{name: "offset normalized without mutation", before: offsetPosition, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "minimum database time", before: &domain.FileEntryPosition{ModifiedAt: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), ID: 20}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{}}},
		{name: "maximum database time", before: &domain.FileEntryPosition{ModifiedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), ID: 20}, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "maximum page size", pageSize: 100, entries: fullEntries, want: &domain.FileEntryPage{Entries: fullEntries[:100], HasMore: true}},
		{name: "full unsigned values", userID: maximum, parentID: maximum, entries: []domain.FileEntry{large}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{large}}},
		{name: "query failure", queryError: queryFailure, wantError: queryFailure},
		{name: "partial row failure", entries: []domain.FileEntry{base}, rowError: rowFailure, wantError: rowFailure},
		{name: "wrong owner", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][1] = "8" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "wrong parent", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][2] = "12" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "null parent", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][2] = nil }, wantError: domain.ErrInvalidFileEntryData},
		{name: "inactive row", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][7] = "TRASHED" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero id", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][0] = "0" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero revision", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][6] = "0" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero time", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][9] = time.Time{} }, wantError: domain.ErrInvalidFileEntryData},
		{name: "submicrosecond result", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][9] = stamp.Add(time.Nanosecond) }, wantError: domain.ErrInvalidFileEntryData},
		{name: "invalid id scan", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][0] = "bad-id" }},
		{name: "invalid time scan", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][9] = "bad-time" }},
		{name: "ascending time", entries: []domain.FileEntry{olderTime, base}, wantError: domain.ErrInvalidFileEntryData},
		{name: "ascending tie ids", entries: []domain.FileEntry{sameTime, base}, wantError: domain.ErrInvalidFileEntryData},
		{name: "duplicate row", entries: []domain.FileEntry{base, base}, wantError: domain.ErrInvalidFileEntryData},
		{name: "bound id repeated", before: &domain.FileEntryPosition{ModifiedAt: stamp, ID: 19}, entries: []domain.FileEntry{base}, wantError: domain.ErrInvalidFileEntryData},
		{name: "newer than time bound", before: &domain.FileEntryPosition{ModifiedAt: stamp.Add(-time.Second), ID: 100}, entries: []domain.FileEntry{base}, wantError: domain.ErrInvalidFileEntryData},
		{name: "invalid extra row", pageSize: 1, entries: []domain.FileEntry{base, sameTime}, change: func(rows [][]driver.Value) { rows[1][2] = "12" }, wantError: domain.ErrInvalidFileEntryData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备结果，核对十一列、复合翻页条件与全部绑定参数。
			userID, parentID, pageSize := testCase.userID, testCase.parentID, testCase.pageSize
			if userID == 0 {
				userID = 7
			}
			if parentID == 0 {
				parentID = 11
			}
			if pageSize == 0 {
				pageSize = 2
			}
			var beforeCopy domain.FileEntryPosition
			if testCase.before != nil {
				beforeCopy = *testCase.before
			}
			var records [][]driver.Value
			for _, entry := range testCase.entries {
				records = append(records, directoryEntryValues(entry))
			}
			if testCase.change != nil {
				testCase.change(records)
			}
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				head := "SELECT id,user_id,parent_id,name,entry_type,file_size,revision,status,created_at,modified_at,updated_at FROM file_entries WHERE "
				predicate := "user_id = ? AND parent_id = ? AND status = ?"
				values := []any{userID, parentID, "ACTIVE"}
				if testCase.before != nil {
					predicate = "(" + predicate + ") AND ((modified_at < ? OR (modified_at = ? AND id < ?)))"
					utc := testCase.before.ModifiedAt.UTC()
					values = append(values, utc, utc, testCase.before.ID)
				}
				values = append(values, int64(pageSize+1))
				expectedArguments := make([]driver.NamedValue, len(values))
				for index, value := range values {
					expectedArguments[index] = driver.NamedValue{Ordinal: index + 1, Value: value}
				}
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != head+predicate+" ORDER BY modified_at DESC, id DESC LIMIT ?" || !reflect.DeepEqual(arguments, expectedArguments) {
					t.Error("目录分页投影、范围、排序或绑定参数错误", actual)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("查询缺少期限")
				}
				return &directoryEntryRows{records: records, terminalError: testCase.rowError}, testCase.queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 核对完整页、HasMore 或错误，失败时丢弃部分结果。
			page, err := store.ListUserDirectoryEntries(context.Background(), userID, parentID, testCase.before, pageSize)
			if testCase.want != nil {
				if err != nil || !reflect.DeepEqual(page, testCase.want) {
					t.Fatal("目录项字段或分页结果错误", err)
				}
			} else if page != nil || err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
				t.Fatal("失败时未返回空页或丢失原因", err)
			}
			if queries != 1 {
				t.Fatal("分页查询未恰好执行一次")
			}
			if testCase.before != nil && !reflect.DeepEqual(*testCase.before, beforeCopy) {
				t.Fatal("调用修改了输入位置")
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池")
			}
		})
	}
}

func TestListUserDirectoryEntriesInput(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer release()
	for _, testCase := range []struct {
		name             string
		ctx              context.Context
		userID, parentID uint64
		size             int
		before           *domain.FileEntryPosition
		want             error
	}{
		{name: "nil context", userID: 7, parentID: 11, size: 2},
		{name: "zero user", ctx: context.Background(), parentID: 11, size: 2},
		{name: "zero parent", ctx: context.Background(), userID: 7, size: 2},
		{name: "zero size", ctx: context.Background(), userID: 7, parentID: 11},
		{name: "negative size", ctx: context.Background(), userID: 7, parentID: 11, size: -1},
		{name: "oversize", ctx: context.Background(), userID: 7, parentID: 11, size: 101},
		{name: "cancelled", ctx: cancelled, userID: 7, parentID: 11, size: 2, want: context.Canceled},
		{name: "expired", ctx: expired, userID: 7, parentID: 11, size: 2, want: context.DeadlineExceeded},
		{name: "zero position id", ctx: context.Background(), userID: 7, parentID: 11, size: 2, before: &domain.FileEntryPosition{ModifiedAt: stamp}},
		{name: "zero position time", ctx: context.Background(), userID: 7, parentID: 11, size: 2, before: &domain.FileEntryPosition{ID: 19}},
		{name: "submicrosecond position", ctx: context.Background(), userID: 7, parentID: 11, size: 2, before: &domain.FileEntryPosition{ModifiedAt: stamp.Add(time.Nanosecond), ID: 19}},
		{name: "position too old", ctx: context.Background(), userID: 7, parentID: 11, size: 2, before: &domain.FileEntryPosition{ModifiedAt: time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC), ID: 19}},
		{name: "position too new", ctx: context.Background(), userID: 7, parentID: 11, size: 2, before: &domain.FileEntryPosition{ModifiedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), ID: 19}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			connector := &deviceConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserDirectoryEntries(testCase.ctx, testCase.userID, testCase.parentID, testCase.before, testCase.size)
			if page != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || connector.connections.Load() != 0 {
				t.Fatal("非法输入或取消仍访问数据库", err)
			}
		})
	}
}

func TestListUserDirectoryEntriesCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after rows", "after missing", "after failure"} {
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
						return nil, errors.New("fixture failure")
					}
					if phase == "after rows" {
						parent := uint64(11)
						stamp := time.Now().UTC().Truncate(time.Microsecond)
						entry := domain.FileEntry{ID: 19, UserID: 7, ParentID: &parent, Name: "目录", EntryType: "DIR", Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp, UpdatedAt: stamp}
						return &directoryEntryRows{records: [][]driver.Value{directoryEntryValues(entry)}}, nil
					}
					return &directoryEntryRows{}, nil
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
			page, err := store.ListUserDirectoryEntries(ctx, 7, 11, nil, 2)
			want := error(context.DeadlineExceeded)
			if strings.HasPrefix(phase, "after") {
				want = context.Canceled
			}
			if page != nil || !errors.Is(err, want) {
				t.Fatal("取消后返回页或丢失原因", err)
			}
		})
	}
}
