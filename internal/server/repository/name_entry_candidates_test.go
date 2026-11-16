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

type nameEntryRows struct{ *directoryEntryRows }

func (*nameEntryRows) Columns() []string {
	return append((&fileEntryRows{}).Columns(), "name_key")
}

func nameEntryCandidateQuery(size int) domain.GlobalEntryQuery {
	return domain.GlobalEntryQuery{Scope: "all", Sort: "name_asc", PageSize: size}
}

func nameEntryCandidateFixture() domain.NameEntryCandidate {
	stamp := time.Date(2026, 11, 17, 2, 30, 0, 123456000, time.UTC)
	parentID := uint64(11)
	return domain.NameEntryCandidate{
		Entry:   domain.FileEntry{ID: 19, UserID: 7, ParentID: &parentID, Name: "B.MD", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp.Add(-time.Hour), ModifiedAt: stamp, UpdatedAt: stamp},
		NameKey: []byte("b.md"),
	}
}

func TestListUserNameEntryCandidatesResults(t *testing.T) {
	// 1. 准备同键不同 ID、不同键反向 ID 及完整二进制边界。
	base := nameEntryCandidateFixture()
	tied, later, directory := base, base, base
	tied.Entry.ID = 20
	later.Entry.ID, later.NameKey = 1, []byte("c.md")
	directory.Entry.EntryType, directory.Entry.FileSize = "DIR", 0
	maximum := ^uint64(0)
	large := base
	largeParent := maximum - 1
	large.Entry.ID, large.Entry.UserID, large.Entry.ParentID, large.Entry.Revision, large.Entry.FileSize = maximum, maximum, &largeParent, maximum, maximum
	large.NameKey = bytes.Repeat([]byte{0xff}, 768)
	var full []domain.NameEntryCandidate
	for index := 0; index < 101; index++ {
		candidate := base
		candidate.Entry.ID = uint64(100 + index)
		full = append(full, candidate)
	}
	queryFailure, rowFailure := errors.New("fixture query"), errors.New("fixture partial rows")
	for _, testCase := range []struct {
		name, scope, kind, search, pattern string
		entries                            []domain.NameEntryCandidate
		after                              *domain.NameEntryPosition
		pageSize                           int
		userID                             uint64
		change                             func([][]driver.Value)
		queryError, rowError, wantError    error
		fail                               bool
	}{
		{name: "empty"},
		{name: "file", entries: []domain.NameEntryCandidate{base}},
		{name: "directory", entries: []domain.NameEntryCandidate{directory}},
		{name: "same key ascending IDs", entries: []domain.NameEntryCandidate{base, tied}},
		{name: "key before ID", entries: []domain.NameEntryCandidate{base, later}},
		{name: "extra candidate", entries: []domain.NameEntryCandidate{base, tied, later}},
		{name: "seek same key", after: &domain.NameEntryPosition{NameKey: []byte("b.md"), ID: 18}, entries: []domain.NameEntryCandidate{base}},
		{name: "seek larger key smaller ID", after: &domain.NameEntryPosition{NameKey: []byte("b.md"), ID: 99}, entries: []domain.NameEntryCandidate{later}},
		{name: "raw binary seek", after: &domain.NameEntryPosition{NameKey: []byte{0, 0xff}, ID: maximum}},
		{name: "maximum values", userID: maximum, entries: []domain.NameEntryCandidate{large}},
		{name: "maximum page", pageSize: 100, entries: full},
		{name: "starred", scope: "starred", entries: []domain.NameEntryCandidate{base, tied}},
		{name: "starred seek filters", scope: "starred", kind: "document", search: "B", pattern: "%B%", after: &domain.NameEntryPosition{NameKey: []byte("a"), ID: maximum}, entries: []domain.NameEntryCandidate{base}},
		{name: "folder", kind: "folder", entries: []domain.NameEntryCandidate{directory}},
		{name: "image", kind: "image", entries: []domain.NameEntryCandidate{base}},
		{name: "video", kind: "video"},
		{name: "audio", kind: "audio"},
		{name: "archive", kind: "archive"},
		{name: "other", kind: "other"},
		{name: "literal pattern", search: "=%_\\?", pattern: "%===%=_\\?%"},
		{name: "search whitespace", search: " 学习 ", pattern: "% 学习 %"},
		{name: "query failure", queryError: queryFailure, wantError: queryFailure, fail: true},
		{name: "partial rows failure", entries: []domain.NameEntryCandidate{base}, rowError: rowFailure, wantError: rowFailure, fail: true},
		{name: "wrong owner", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][1] = "8" }, fail: true},
		{name: "nil parent", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][2] = nil }, fail: true},
		{name: "zero parent", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][2] = "0" }, fail: true},
		{name: "self parent", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][2] = "19" }, fail: true},
		{name: "empty name", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][3] = "" }, fail: true},
		{name: "invalid name UTF-8", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][3] = string([]byte{0xff}) }, fail: true},
		{name: "invalid type", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][4] = "LINK" }, fail: true},
		{name: "folder receives file", kind: "folder", entries: []domain.NameEntryCandidate{base}, fail: true},
		{name: "file category receives directory", kind: "document", entries: []domain.NameEntryCandidate{directory}, fail: true},
		{name: "directory size", entries: []domain.NameEntryCandidate{directory}, change: func(rows [][]driver.Value) { rows[0][5] = "1" }, fail: true},
		{name: "zero revision", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][6] = "0" }, fail: true},
		{name: "inactive", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][7] = "TRASHED" }, fail: true},
		{name: "zero ID", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][0] = "0" }, fail: true},
		{name: "zero created", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][8] = time.Time{} }, fail: true},
		{name: "submicrosecond modified", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][9] = base.Entry.ModifiedAt.Add(time.Nanosecond) }, fail: true},
		{name: "zero audit", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][10] = time.Time{} }, fail: true},
		{name: "nil key", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = nil }, fail: true},
		{name: "empty key", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = []byte{} }, fail: true},
		{name: "oversized key", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][11] = bytes.Repeat([]byte{'a'}, 769) }, fail: true},
		{name: "descending keys", entries: []domain.NameEntryCandidate{later, base}, fail: true},
		{name: "descending tie IDs", entries: []domain.NameEntryCandidate{tied, base}, fail: true},
		{name: "duplicate tuple", entries: []domain.NameEntryCandidate{base, base}, fail: true},
		{name: "repeated bound", after: &domain.NameEntryPosition{NameKey: base.NameKey, ID: base.Entry.ID}, entries: []domain.NameEntryCandidate{base}, fail: true},
		{name: "below bound", after: &domain.NameEntryPosition{NameKey: []byte("c"), ID: 1}, entries: []domain.NameEntryCandidate{base}, fail: true},
		{name: "invalid peek", pageSize: 1, entries: []domain.NameEntryCandidate{base, tied}, change: func(rows [][]driver.Value) { rows[1][11] = nil }, fail: true},
		{name: "too many rows", pageSize: 1, entries: []domain.NameEntryCandidate{base, tied, later}, fail: true},
		{name: "scan failure", entries: []domain.NameEntryCandidate{base}, change: func(rows [][]driver.Value) { rows[0][0] = "bad-id" }, fail: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 精确核对十二列投影、绑定筛选和复合升序条件。
			userID, size := testCase.userID, testCase.pageSize
			if userID == 0 {
				userID = 7
			}
			if size == 0 {
				size = 2
			}
			query := nameEntryCandidateQuery(size)
			query.Kind, query.Search = testCase.kind, testCase.search
			if testCase.scope != "" {
				query.Scope = testCase.scope
			}
			var records [][]driver.Value
			for _, candidate := range testCase.entries {
				records = append(records, append(directoryEntryValues(candidate.Entry), candidate.NameKey))
			}
			if testCase.change != nil {
				testCase.change(records)
			}
			calls := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				calls++
				predicates := []string{"user_id = ? AND status = ? AND parent_id IS NOT NULL"}
				values := []any{userID, "ACTIVE"}
				if query.Scope == "starred" || query.Kind != "" || query.Search != "" || testCase.after != nil {
					predicates[0] = "(" + predicates[0] + ")"
				}
				if query.Scope == "starred" {
					predicates = append(predicates, "(EXISTS (SELECT 1 FROM user_entry_preferences WHERE user_entry_preferences.user_id = ? AND user_entry_preferences.entry_id = file_entries.id AND user_entry_preferences.starred_at IS NOT NULL))")
					values = append(values, userID)
				}
				if query.Kind == "folder" {
					predicates = append(predicates, "entry_type = ?")
					values = append(values, "DIR")
				} else if query.Kind != "" {
					predicates = append(predicates, "(entry_type = ? AND category = ?)")
					values = append(values, "FILE", strings.ToUpper(query.Kind))
				}
				if query.Search != "" {
					predicates = append(predicates, "name LIKE ? ESCAPE '='")
					values = append(values, testCase.pattern)
				}
				if testCase.after != nil {
					predicates = append(predicates, "((name_key > ? OR (name_key = ? AND id > ?)))")
					values = append(values, testCase.after.NameKey, testCase.after.NameKey, testCase.after.ID)
				}
				values = append(values, int64(size+1))
				expectedArguments := make([]driver.NamedValue, len(values))
				for index, value := range values {
					expectedArguments[index] = driver.NamedValue{Ordinal: index + 1, Value: value}
				}
				expected := "SELECT id,user_id,parent_id,name,entry_type,file_size,revision,status,created_at,modified_at,updated_at,name_key FROM file_entries WHERE " + strings.Join(predicates, " AND ") + " ORDER BY name_key ASC, id ASC LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || !reflect.DeepEqual(arguments, expectedArguments) {
					t.Error("名称候选 SQL 或绑定参数错误", actual)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("名称候选查询期限错误")
				}
				return &nameEntryRows{&directoryEntryRows{records: records, terminalError: testCase.rowError}}, testCase.queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserNameEntryCandidates(context.Background(), userID, query, testCase.after)
			if testCase.fail {
				if page != nil || err == nil || (testCase.wantError != nil && !errors.Is(err, testCase.wantError)) {
					t.Fatal("名称候选失败结果或错误链错误", err)
				}
			} else {
				count := min(size, len(testCase.entries))
				if err != nil || page == nil || page.Entries == nil || len(page.Entries) != count || page.HasMore != (len(testCase.entries) > size) {
					t.Fatal("名称候选分页形状错误", err)
				}
				for index, candidate := range page.Entries {
					if !reflect.DeepEqual(candidate, testCase.entries[index]) {
						t.Fatal("名称候选完整字段错误")
					}
				}
			}
			if calls != 1 {
				t.Fatal("名称候选查询次数错误")
			}
		})
	}
}

func TestListUserNameEntryCandidatesInputs(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero query", "directory scope", "recent scope", "modified sort", "size sort", "zero size", "large size", "invalid search", "invalid kind", "uppercase kind", "zero position ID", "empty position key", "oversized position key", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 非法参数和预先取消在建立数据库连接前拒绝。
			var ctx context.Context = context.Background()
			query := nameEntryCandidateQuery(2)
			userID := uint64(7)
			var after *domain.NameEntryPosition
			var expected error
			switch mode {
			case "nil context":
				ctx = nil
			case "zero user":
				userID = 0
			case "zero query":
				query = domain.GlobalEntryQuery{}
			case "directory scope":
				query.Scope = "directory"
			case "recent scope":
				query.Scope, query.Sort = "recent", "last_opened_desc"
			case "modified sort":
				query.Sort = "modified_desc"
			case "size sort":
				query.Sort = "size_desc"
			case "zero size":
				query.PageSize = 0
			case "large size":
				query.PageSize = 101
			case "invalid search":
				query.Search = string([]byte{0xff})
			case "invalid kind":
				query.Kind = "invalid-kind"
			case "uppercase kind":
				query.Kind = "DOCUMENT"
			case "zero position ID":
				after = &domain.NameEntryPosition{NameKey: []byte("a")}
			case "empty position key":
				after = &domain.NameEntryPosition{ID: 19}
			case "oversized position key":
				after = &domain.NameEntryPosition{NameKey: bytes.Repeat([]byte{'a'}, 769), ID: 19}
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, expected = cancelled, context.Canceled
			case "expired":
				expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				ctx, expected = expired, context.DeadlineExceeded
			}
			connector := &deviceConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserNameEntryCandidates(ctx, userID, query, after)
			if page != nil || err == nil || (expected != nil && !errors.Is(err, expected)) || connector.connections.Load() != 0 {
				t.Fatal("名称候选非法输入仍查询数据库", err)
			}
		})
	}
}

func TestListUserNameEntryCandidatesCancellation(t *testing.T) {
	for _, mode := range []string{"caller deadline", "default deadline", "after rows", "after failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "caller deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 30*time.Millisecond)
				defer release()
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				// 1. 验证调用者和默认期限，并让取消优先于已有结果或故障。
				if strings.HasPrefix(mode, "after") {
					cancel()
					if mode == "after failure" {
						return nil, errors.New("fixture failure")
					}
					candidate := nameEntryCandidateFixture()
					return &nameEntryRows{&directoryEntryRows{records: [][]driver.Value{append(directoryEntryValues(candidate.Entry), candidate.NameKey)}}}, nil
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
			page, err := store.ListUserNameEntryCandidates(ctx, 7, nameEntryCandidateQuery(2), nil)
			expected := context.Canceled
			if strings.HasSuffix(mode, "deadline") {
				expected = context.DeadlineExceeded
			}
			if page != nil || !errors.Is(err, expected) {
				t.Fatal("名称候选取消结果或错误链错误", err)
			}
		})
	}
}

func TestListUserNameEntryCandidatesCopies(t *testing.T) {
	// 1. 驱动查询时修改来源位置，验证绑定键及校验边界已独立复制。
	after := &domain.NameEntryPosition{NameKey: []byte("a.md"), ID: 18}
	candidate := nameEntryCandidateFixture()
	connector := &deviceConnector{accountConnector: accountConnector{query: func(_ context.Context, _ string, arguments []driver.NamedValue) (driver.Rows, error) {
		if len(arguments) == 6 {
			after.NameKey[0] = 'z'
			if !bytes.Equal(arguments[2].Value.([]byte), []byte("a.md")) || !bytes.Equal(arguments[3].Value.([]byte), []byte("a.md")) {
				t.Error("绑定键与来源位置共享字节")
			}
		}
		other := candidate
		other.Entry.ID = 20
		return &nameEntryRows{&directoryEntryRows{records: [][]driver.Value{append(directoryEntryValues(candidate.Entry), candidate.NameKey), append(directoryEntryValues(other.Entry), candidate.NameKey)}}}, nil
	}}}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ListUserNameEntryCandidates(context.Background(), 7, nameEntryCandidateQuery(2), after)
	if err != nil || first == nil || len(first.Entries) != 2 {
		t.Fatal("名称输入位置副本未保护当前读取", err)
	}

	// 2. 修改一项返回键和父目录，核对其他行、驱动来源及再次读取。
	first.Entries[0].NameKey[0] = 'x'
	*first.Entries[0].Entry.ParentID = 999
	second, err := store.ListUserNameEntryCandidates(context.Background(), 7, nameEntryCandidateQuery(2), nil)
	if err != nil || second == nil || string(first.Entries[1].NameKey) != "b.md" || *first.Entries[1].Entry.ParentID != 11 || string(candidate.NameKey) != "b.md" || *candidate.Entry.ParentID != 11 || string(second.Entries[0].NameKey) != "b.md" || *second.Entries[0].Entry.ParentID != 11 {
		t.Fatal("名称候选结果副本相互影响", err)
	}
}
