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

// globalEntryCandidateQuery 准备当前阶段已支持的全局候选查询。
func globalEntryCandidateQuery(pageSize int) domain.GlobalEntryQuery {
	return domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeAll, Sort: domain.GlobalEntrySortModifiedDesc, PageSize: pageSize}
}

func TestListUserGlobalEntryCandidatesResults(t *testing.T) {
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
	maximumParent := maximum - 1
	large.ID, large.UserID, large.ParentID, large.FileSize, large.Revision = maximum, maximum, &maximumParent, maximum, maximum
	otherParent := uint64(12)
	otherDirectory := sameTime
	otherDirectory.ParentID = &otherParent
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
		userID                          uint64
		change                          func([][]driver.Value)
		queryError, rowError, wantError error
		want                            *domain.FileEntryPage
		kind                            string
	}{
		{name: "empty page", want: &domain.FileEntryPage{Entries: []domain.FileEntry{}}},
		{name: "single file", entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "directory", entries: []domain.FileEntry{directory}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{directory}}},
		{name: "different parents", entries: []domain.FileEntry{base, otherDirectory}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, otherDirectory}}},
		{name: "same timestamp ordered by id", entries: []domain.FileEntry{base, sameTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, sameTime}}},
		{name: "timestamp before id", entries: []domain.FileEntry{base, olderTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, olderTime}}},
		{name: "extra row", entries: []domain.FileEntry{base, sameTime, olderTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, sameTime}, HasMore: true}},
		{name: "seek same timestamp", before: position, entries: []domain.FileEntry{base, sameTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base, sameTime}}},
		{name: "seek older timestamp larger id", before: position, entries: []domain.FileEntry{olderTime}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{olderTime}}},
		{name: "offset normalized without mutation", before: offsetPosition, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "minimum database time", before: &domain.FileEntryPosition{ModifiedAt: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), ID: 20}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{}}},
		{name: "maximum database time", before: &domain.FileEntryPosition{ModifiedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), ID: 20}, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "maximum page size", pageSize: 100, entries: fullEntries, want: &domain.FileEntryPage{Entries: fullEntries[:100], HasMore: true}},
		{name: "full unsigned values", userID: maximum, entries: []domain.FileEntry{large}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{large}}},
		{name: "query failure", queryError: queryFailure, wantError: queryFailure},
		{name: "partial row failure", entries: []domain.FileEntry{base}, rowError: rowFailure, wantError: rowFailure},
		{name: "wrong owner", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][1] = "8" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "null parent", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][2] = nil }, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero parent", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][2] = "0" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "self parent", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][2] = "19" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "empty name", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][3] = "" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "invalid name encoding", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][3] = string([]byte{0xff}) }, wantError: domain.ErrInvalidFileEntryData},
		{name: "unknown type", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][4] = "LINK" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "directory with size", entries: []domain.FileEntry{directory}, change: func(rows [][]driver.Value) { rows[0][5] = "1" }, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero creation time", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][8] = time.Time{} }, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero audit time", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][10] = time.Time{} }, wantError: domain.ErrInvalidFileEntryData},
		{name: "too many rows", pageSize: 1, entries: []domain.FileEntry{base, sameTime, olderTime}, wantError: domain.ErrInvalidFileEntryData},
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
		{name: "folder filter first", kind: "folder", entries: []domain.FileEntry{directory}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{directory}}},
		{name: "folder filter seek", kind: "folder", before: position, entries: []domain.FileEntry{directory}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{directory}}},
		{name: "document filter first", kind: "document", entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "document filter seek", kind: "document", before: position, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "image filter first", kind: "image", entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "image filter seek", kind: "image", before: position, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "video filter first", kind: "video", entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "video filter seek", kind: "video", before: position, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "audio filter first", kind: "audio", entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "audio filter seek", kind: "audio", before: position, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "archive filter first", kind: "archive", entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "archive filter seek", kind: "archive", before: position, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "other filter first", kind: "other", entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "other filter seek", kind: "other", before: position, entries: []domain.FileEntry{base}, want: &domain.FileEntryPage{Entries: []domain.FileEntry{base}}},
		{name: "folder filter receives file", kind: "folder", entries: []domain.FileEntry{base}, wantError: domain.ErrInvalidFileEntryData},
		{name: "file filter receives directory", kind: "document", entries: []domain.FileEntry{directory}, wantError: domain.ErrInvalidFileEntryData},
		{name: "folder extra row receives file", kind: "folder", pageSize: 1, entries: []domain.FileEntry{directory, sameTime}, wantError: domain.ErrInvalidFileEntryData},
		{name: "empty folder filter", kind: "folder", want: &domain.FileEntryPage{Entries: []domain.FileEntry{}}},
		{name: "invalid extra row", pageSize: 1, entries: []domain.FileEntry{base, sameTime}, change: func(rows [][]driver.Value) { rows[1][2] = "0" }, wantError: domain.ErrInvalidFileEntryData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备结果，核对十一列、复合翻页条件与全部绑定参数。
			userID, pageSize := testCase.userID, testCase.pageSize
			if userID == 0 {
				userID = 7
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
				predicates := []string{"user_id = ? AND status = ? AND parent_id IS NOT NULL"}
				values := []any{userID, "ACTIVE"}
				if testCase.kind != "" || testCase.before != nil {
					predicates[0] = "(" + predicates[0] + ")"
				}
				if testCase.kind == "folder" {
					predicates = append(predicates, "entry_type = ?")
					values = append(values, "DIR")
				} else if testCase.kind != "" {
					predicates = append(predicates, "(entry_type = ? AND category = ?)")
					values = append(values, "FILE", strings.ToUpper(testCase.kind))
				}
				if testCase.before != nil {
					predicates = append(predicates, "((modified_at < ? OR (modified_at = ? AND id < ?)))")
					utc := testCase.before.ModifiedAt.UTC()
					values = append(values, utc, utc, testCase.before.ID)
				}
				predicate := strings.Join(predicates, " AND ")
				values = append(values, int64(pageSize+1))
				expectedArguments := make([]driver.NamedValue, len(values))
				for index, value := range values {
					expectedArguments[index] = driver.NamedValue{Ordinal: index + 1, Value: value}
				}
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != head+predicate+" ORDER BY modified_at DESC, id DESC LIMIT ?" || !reflect.DeepEqual(arguments, expectedArguments) {
					t.Error("全局候选分页投影、范围、排序或绑定参数错误", actual)
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
			query := globalEntryCandidateQuery(pageSize)
			query.Kind = testCase.kind
			queryBefore := query
			page, err := store.ListUserGlobalEntryCandidates(context.Background(), userID, query, testCase.before)
			if query != queryBefore {
				t.Fatal("读取修改了查询条件")
			}
			if testCase.want != nil {
				if err != nil || !reflect.DeepEqual(page, testCase.want) {
					t.Fatal("候选字段或分页结果错误", err)
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

func TestListUserGlobalEntryCandidatesInput(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer release()
	for _, testCase := range []struct {
		name   string
		ctx    context.Context
		userID uint64
		size   int
		before *domain.FileEntryPosition
		want   error
	}{
		{name: "nil context", userID: 7, size: 2},
		{name: "zero user", ctx: context.Background(), size: 2},
		{name: "zero size", ctx: context.Background(), userID: 7},
		{name: "negative size", ctx: context.Background(), userID: 7, size: -1},
		{name: "oversize", ctx: context.Background(), userID: 7, size: 101},
		{name: "cancelled", ctx: cancelled, userID: 7, size: 2, want: context.Canceled},
		{name: "expired", ctx: expired, userID: 7, size: 2, want: context.DeadlineExceeded},
		{name: "zero position id", ctx: context.Background(), userID: 7, size: 2, before: &domain.FileEntryPosition{ModifiedAt: stamp}},
		{name: "zero position time", ctx: context.Background(), userID: 7, size: 2, before: &domain.FileEntryPosition{ID: 19}},
		{name: "submicrosecond position", ctx: context.Background(), userID: 7, size: 2, before: &domain.FileEntryPosition{ModifiedAt: stamp.Add(time.Nanosecond), ID: 19}},
		{name: "position too old", ctx: context.Background(), userID: 7, size: 2, before: &domain.FileEntryPosition{ModifiedAt: time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC), ID: 19}},
		{name: "position too new", ctx: context.Background(), userID: 7, size: 2, before: &domain.FileEntryPosition{ModifiedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), ID: 19}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			connector := &deviceConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserGlobalEntryCandidates(testCase.ctx, testCase.userID, globalEntryCandidateQuery(testCase.size), testCase.before)
			if page != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || connector.connections.Load() != 0 {
				t.Fatal("非法输入或取消仍访问数据库", err)
			}
		})
	}
}

func TestListUserGlobalEntryCandidatesQueryBounds(t *testing.T) {
	for _, mode := range []string{"zero query", "directory scope", "starred scope", "recent scope", "name sort", "size sort", "nonempty search", "invalid encoding", "invalid kind", "uppercase kind", "padded kind", "raw file kind", "injection kind"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 检查当前读取能力支持的查询组合，提前拒绝其余条件。
			query := globalEntryCandidateQuery(2)
			switch mode {
			case "zero query":
				query = domain.GlobalEntryQuery{}
			case "directory scope":
				query.Scope = "directory"
			case "starred scope":
				query.Scope = domain.GlobalEntryScopeStarred
			case "recent scope":
				query.Scope, query.Sort = domain.GlobalEntryScopeRecent, domain.GlobalEntrySortLastOpenedDesc
			case "name sort":
				query.Sort = domain.GlobalEntrySortNameAsc
			case "size sort":
				query.Sort = domain.GlobalEntrySortSizeDesc
			case "nonempty search":
				query.Search = "笔记"
			case "invalid encoding":
				query.Search = string([]byte{0xff})
			case "invalid kind":
				query.Kind = "private-kind"
			case "uppercase kind":
				query.Kind = "DOCUMENT"
			case "padded kind":
				query.Kind = " document "
			case "raw file kind":
				query.Kind = "FILE"
			case "injection kind":
				query.Kind = "document' OR 1=1 --"
			}
			connector := &deviceConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserGlobalEntryCandidates(context.Background(), 7, query, nil)
			if page != nil || err == nil || connector.connections.Load() != 0 {
				t.Fatal("未支持的查询条件访问了数据库")
			}
		})
	}
}

func TestListUserGlobalEntryCandidatesCancellation(t *testing.T) {
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
			page, err := store.ListUserGlobalEntryCandidates(ctx, 7, globalEntryCandidateQuery(2), nil)
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
