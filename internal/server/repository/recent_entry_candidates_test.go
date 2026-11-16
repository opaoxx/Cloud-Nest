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

// recentEntryRows 在既有十一列文件夹具后追加访问时间列。
type recentEntryRows struct{ *directoryEntryRows }

func (*recentEntryRows) Columns() []string {
	return append((&fileEntryRows{}).Columns(), "last_opened_at")
}

func recentEntryCandidateQuery(pageSize int) domain.GlobalEntryQuery {
	return domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeRecent, Sort: domain.GlobalEntrySortLastOpenedDesc, PageSize: pageSize}
}

func TestListUserRecentEntryCandidatesResults(t *testing.T) {
	// 1. 准备修改时间与访问时间顺序不同的文件，并复用已有行驱动。
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	parentID := uint64(11)
	base := domain.RecentEntryCandidate{
		Entry:        domain.FileEntry{ID: 19, UserID: 7, ParentID: &parentID, Name: "学习笔记 🌱.md", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp.Add(-time.Hour), ModifiedAt: stamp, UpdatedAt: stamp},
		LastOpenedAt: stamp.Add(time.Hour),
	}
	tied, older := base, base
	tied.Entry.ID = 18
	older.Entry.ID, older.LastOpenedAt, older.Entry.ModifiedAt = 99, base.LastOpenedAt.Add(-time.Second), stamp.Add(time.Second)
	before := &domain.RecentEntryPosition{LastOpenedAt: base.LastOpenedAt.In(time.FixedZone("fixture", 8*3600)), ID: 20}
	queryFailure, rowFailure := errors.New("fixture query"), errors.New("fixture partial rows")
	for _, testCase := range []struct {
		name       string
		kind       string
		entries    []domain.RecentEntryCandidate
		before     *domain.RecentEntryPosition
		size       int
		change     func([][]driver.Value)
		queryError error
		rowError   error
		wantError  error
		invalid    bool
		more       bool
	}{
		{name: "document filter", kind: "document", entries: []domain.RecentEntryCandidate{base}},
		{name: "image filter", kind: "image", entries: []domain.RecentEntryCandidate{base}},
		{name: "video filter", kind: "video", entries: []domain.RecentEntryCandidate{base}},
		{name: "audio filter", kind: "audio", entries: []domain.RecentEntryCandidate{base}},
		{name: "archive filter", kind: "archive", entries: []domain.RecentEntryCandidate{base}},
		{name: "other filter", kind: "other", entries: []domain.RecentEntryCandidate{base}},
		{name: "folder empty intersection", kind: "folder"},
		{name: "folder seek", kind: "folder", before: before},
		{name: "folder unexpected row", kind: "folder", entries: []domain.RecentEntryCandidate{base}, invalid: true},
		{name: "document seek and peek", kind: "document", before: before, entries: []domain.RecentEntryCandidate{base, tied, older}, more: true},
		{name: "empty image result", kind: "image"},
		{name: "classified query failure", kind: "document", queryError: queryFailure, wantError: queryFailure},
		{name: "folder query failure", kind: "folder", queryError: queryFailure, wantError: queryFailure},
		{name: "classified invalid peek", kind: "document", size: 1, entries: []domain.RecentEntryCandidate{base, tied}, change: func(rows [][]driver.Value) { rows[1][11] = nil }, invalid: true},
		{name: "empty"},
		{name: "single file", entries: []domain.RecentEntryCandidate{base}},
		{name: "same access timestamp", entries: []domain.RecentEntryCandidate{base, tied}},
		{name: "access time before modification and ID", entries: []domain.RecentEntryCandidate{base, older}},
		{name: "extra row", entries: []domain.RecentEntryCandidate{base, tied, older}, more: true},
		{name: "seek and UTC copy", entries: []domain.RecentEntryCandidate{base, tied}, before: before},
		{name: "query failure", queryError: queryFailure, wantError: queryFailure},
		{name: "partial row failure", entries: []domain.RecentEntryCandidate{base}, rowError: rowFailure, wantError: rowFailure},
		{name: "wrong owner", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][1] = "8" }, invalid: true},
		{name: "root", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][2] = nil }, invalid: true},
		{name: "zero parent", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][2] = "0" }, invalid: true},
		{name: "self parent", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][2] = "19" }, invalid: true},
		{name: "empty name", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][3] = "" }, invalid: true},
		{name: "invalid UTF-8", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][3] = string([]byte{0xff}) }, invalid: true},
		{name: "directory", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][4] = "DIR" }, invalid: true},
		{name: "zero revision", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][6] = "0" }, invalid: true},
		{name: "inactive", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][7] = "TRASHED" }, invalid: true},
		{name: "zero created time", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][8] = time.Time{} }, invalid: true},
		{name: "invalid modified time", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][9] = stamp.Add(time.Nanosecond) }, invalid: true},
		{name: "zero updated time", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][10] = time.Time{} }, invalid: true},
		{name: "null access time", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = nil }, invalid: true},
		{name: "zero access time", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = time.Time{} }, invalid: true},
		{name: "access nanoseconds", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = stamp.Add(time.Nanosecond) }, invalid: true},
		{name: "access year below minimum", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC) }, invalid: true},
		{name: "access year above maximum", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }, invalid: true},
		{name: "bad access scan", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = "bad-time" }, wantError: errors.New("scan failure")},
		{name: "bad ID scan", entries: []domain.RecentEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][0] = "bad-id" }, wantError: errors.New("scan failure")},
		{name: "ascending access time", entries: []domain.RecentEntryCandidate{older, base}, invalid: true},
		{name: "ascending tie IDs", entries: []domain.RecentEntryCandidate{tied, base}, invalid: true},
		{name: "duplicate position", entries: []domain.RecentEntryCandidate{base, base}, invalid: true},
		{name: "upper bound repeated", before: &domain.RecentEntryPosition{ID: 19, LastOpenedAt: base.LastOpenedAt}, entries: []domain.RecentEntryCandidate{base}, invalid: true},
		{name: "newer than bound", before: &domain.RecentEntryPosition{ID: 100, LastOpenedAt: older.LastOpenedAt}, entries: []domain.RecentEntryCandidate{base}, invalid: true},
		{name: "invalid peek", size: 1, entries: []domain.RecentEntryCandidate{base, tied}, change: func(rows [][]driver.Value) { rows[1][11] = nil }, invalid: true},
		{name: "too many rows", size: 1, entries: []domain.RecentEntryCandidate{base, tied, older}, invalid: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			size := testCase.size
			if size == 0 {
				size = 2
			}
			records := make([][]driver.Value, 0, len(testCase.entries))
			for _, candidate := range testCase.entries {
				records = append(records, append(directoryEntryValues(candidate.Entry), candidate.LastOpenedAt))
			}
			if testCase.change != nil {
				testCase.change(records)
			}
			calls := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				calls++
				expected := "SELECT file_entries.id,file_entries.user_id,file_entries.parent_id,file_entries.name,file_entries.entry_type,file_entries.file_size,file_entries.revision,file_entries.status,file_entries.created_at,file_entries.modified_at,file_entries.updated_at,preferences.last_opened_at FROM file_entries JOIN user_entry_preferences AS preferences ON preferences.user_id = file_entries.user_id AND preferences.entry_id = file_entries.id WHERE "
				predicate := "file_entries.user_id = ? AND file_entries.status = ? AND file_entries.entry_type = ? AND file_entries.parent_id IS NOT NULL AND preferences.last_opened_at IS NOT NULL"
				values := []any{uint64(7), "ACTIVE", "FILE"}
				if testCase.kind != "" || testCase.before != nil {
					predicate = "(" + predicate + ")"
				}
				if testCase.kind == "folder" {
					predicate += " AND 1 = 0"
				} else if testCase.kind != "" {
					predicate += " AND file_entries.category = ?"
					values = append(values, strings.ToUpper(testCase.kind))
				}
				if testCase.before != nil {
					predicate += " AND ((preferences.last_opened_at < ? OR (preferences.last_opened_at = ? AND file_entries.id < ?)))"
					utc := testCase.before.LastOpenedAt.UTC()
					values = append(values, utc, utc, testCase.before.ID)
				}
				values = append(values, int64(size+1))
				expectedArguments := make([]driver.NamedValue, len(values))
				for index, value := range values {
					expectedArguments[index] = driver.NamedValue{Ordinal: index + 1, Value: value}
				}
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected+predicate+" ORDER BY preferences.last_opened_at DESC, file_entries.id DESC LIMIT ?" || !reflect.DeepEqual(arguments, expectedArguments) {
					t.Error("最近访问投影、关联、排序或绑定参数错误", actual, arguments)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("查询缺少五秒期限")
				}
				return &recentEntryRows{&directoryEntryRows{records: records, terminalError: testCase.rowError}}, testCase.queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			query := recentEntryCandidateQuery(size)
			query.Kind = testCase.kind
			queryCopy := query
			var positionCopy domain.RecentEntryPosition
			if testCase.before != nil {
				positionCopy = *testCase.before
			}

			// 2. 核对完整候选、额外记录和失败时的空结果。
			page, err := store.ListUserRecentEntryCandidates(context.Background(), 7, query, testCase.before)
			if testCase.invalid {
				if page != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
					t.Fatal("异常记录未整页拒绝", err)
				}
			} else if testCase.wantError != nil {
				if page != nil || err == nil || (testCase.queryError != nil && !errors.Is(err, queryFailure)) || (testCase.rowError != nil && !errors.Is(err, rowFailure)) {
					t.Fatal("故障交付或错误原因错误", err)
				}
			} else {
				want := append([]domain.RecentEntryCandidate{}, testCase.entries...)
				if testCase.more {
					want = want[:size]
				}
				if err != nil || !reflect.DeepEqual(page, &domain.RecentEntryPage{Entries: want, HasMore: testCase.more}) {
					t.Fatal("最近访问候选字段或页形状错误", err)
				}
			}
			if calls != 1 || query != queryCopy || (testCase.before != nil && *testCase.before != positionCopy) {
				t.Fatal("查询次数或输入保护错误")
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池", err)
			}
		})
	}
}

func TestListUserRecentEntryCandidatesInput(t *testing.T) {
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	query := recentEntryCandidateQuery(2)
	for _, testCase := range []struct {
		name   string
		change func(*domain.GlobalEntryQuery)
		before *domain.RecentEntryPosition
	}{
		{name: "nil context"},
		{name: "zero user"},
		{name: "zero page", change: func(query *domain.GlobalEntryQuery) { query.PageSize = 0 }},
		{name: "negative page", change: func(query *domain.GlobalEntryQuery) { query.PageSize = -1 }},
		{name: "oversize page", change: func(query *domain.GlobalEntryQuery) { query.PageSize = 101 }},
		{name: "all scope", change: func(query *domain.GlobalEntryQuery) { query.Scope = domain.GlobalEntryScopeAll }},
		{name: "starred scope", change: func(query *domain.GlobalEntryQuery) { query.Scope = domain.GlobalEntryScopeStarred }},
		{name: "modified sort", change: func(query *domain.GlobalEntryQuery) { query.Sort = domain.GlobalEntrySortModifiedDesc }},
		{name: "search pending", change: func(query *domain.GlobalEntryQuery) { query.Search = "学习" }},
		{name: "uppercase kind", change: func(query *domain.GlobalEntryQuery) { query.Kind = "DOCUMENT" }},
		{name: "unknown kind", change: func(query *domain.GlobalEntryQuery) { query.Kind = "private-kind" }},
		{name: "padded kind", change: func(query *domain.GlobalEntryQuery) { query.Kind = " document " }},
		{name: "raw file kind", change: func(query *domain.GlobalEntryQuery) { query.Kind = "FILE" }},
		{name: "injection kind", change: func(query *domain.GlobalEntryQuery) { query.Kind = "document' OR 1=1 --" }},
		{name: "invalid UTF-8", change: func(query *domain.GlobalEntryQuery) { query.Search = string([]byte{0xff}) }},
		{name: "zero position ID", before: &domain.RecentEntryPosition{LastOpenedAt: stamp}},
		{name: "zero position time", before: &domain.RecentEntryPosition{ID: 19}},
		{name: "nanosecond position", before: &domain.RecentEntryPosition{ID: 19, LastOpenedAt: stamp.Add(time.Nanosecond)}},
		{name: "early position", before: &domain.RecentEntryPosition{ID: 19, LastOpenedAt: time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{name: "late position", before: &domain.RecentEntryPosition{ID: 19, LastOpenedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 让非法参数在创建数据库连接前返回。
			ctx := context.Background()
			userID := uint64(7)
			if testCase.name == "nil context" {
				ctx = nil
			}
			if testCase.name == "zero user" {
				userID = 0
			}
			input := query
			if testCase.change != nil {
				testCase.change(&input)
			}
			connector := &deviceConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserRecentEntryCandidates(ctx, userID, input, testCase.before)
			if page != nil || err == nil || connector.connections.Load() != 0 {
				t.Fatal("非法查询访问数据库或交付结果", err)
			}
		})
	}
}

func TestListUserRecentEntryCandidatesCancellation(t *testing.T) {
	for _, phase := range []string{"before query", "caller deadline", "after rows", "after failure"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 分别在查询前、等待期间和驱动返回时触发取消。
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before query" {
				cancel()
			}
			if phase == "caller deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 30*time.Millisecond)
				defer release()
			}
			calls := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(current context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				calls++
				if phase == "caller deadline" {
					<-current.Done()
					return nil, current.Err()
				}
				cancel()
				if phase == "after failure" {
					return nil, errors.New("fixture query failure")
				}
				return &recentEntryRows{&directoryEntryRows{}}, nil
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserRecentEntryCandidates(ctx, 7, recentEntryCandidateQuery(2), nil)
			want := error(context.Canceled)
			if phase == "caller deadline" {
				want = context.DeadlineExceeded
			}
			if page != nil || !errors.Is(err, want) || (phase == "before query" && calls != 0) || (phase != "before query" && calls != 1) {
				t.Fatal("取消优先级、期限或查询次数错误", err, calls)
			}
		})
	}
}
