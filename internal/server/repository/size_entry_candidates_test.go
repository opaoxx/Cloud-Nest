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

func sizeEntryCandidateQuery(pageSize int) domain.GlobalEntryQuery {
	return domain.GlobalEntryQuery{Scope: "all", Sort: "size_desc", PageSize: pageSize}
}

func TestListUserSizeEntryCandidatesInputs(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero query", "directory scope", "recent scope", "modified sort", "name sort", "zero page", "large page", "invalid search", "invalid kind", "zero position ID", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 非法输入和预先取消在建立数据库连接前拒绝。
			var ctx context.Context = context.Background()
			query := sizeEntryCandidateQuery(2)
			userID := uint64(7)
			var before *domain.SizeEntryPosition
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
			case "name sort":
				query.Sort = "name_asc"
			case "zero page":
				query.PageSize = 0
			case "large page":
				query.PageSize = 101
			case "invalid search":
				query.Search = string([]byte{0xff})
			case "invalid kind":
				query.Kind = "DOCUMENT"
			case "zero position ID":
				before = &domain.SizeEntryPosition{FileSize: 2048}
			case "cancelled":
				current, cancel := context.WithCancel(ctx)
				cancel()
				ctx, expected = current, context.Canceled
			case "expired":
				current, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				ctx, expected = current, context.DeadlineExceeded
			}
			connector := &deviceConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ListUserSizeEntryCandidates(ctx, userID, query, before)
			if page != nil || err == nil || (expected != nil && !errors.Is(err, expected)) || connector.connections.Load() != 0 {
				t.Fatal("大小候选非法输入仍查询数据库", err)
			}
		})
	}
}

func TestListUserSizeEntryCandidatesCopies(t *testing.T) {
	// 1. 驱动查询时修改来源位置，核对绑定及结果校验使用独立位置。
	before := &domain.SizeEntryPosition{FileSize: 4096, ID: 20}
	entry := nameEntryCandidateFixture().Entry
	connector := &deviceConnector{accountConnector: accountConnector{query: func(_ context.Context, _ string, arguments []driver.NamedValue) (driver.Rows, error) {
		if len(arguments) == 6 {
			before.FileSize, before.ID = 0, 0
			if arguments[2].Value != uint64(4096) || arguments[3].Value != uint64(4096) || arguments[4].Value != uint64(20) {
				t.Error("大小位置绑定错误")
			}
		}
		return &directoryEntryRows{records: [][]driver.Value{directoryEntryValues(entry)}}, nil
	}}}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ListUserSizeEntryCandidates(context.Background(), 7, sizeEntryCandidateQuery(2), before)
	if err != nil || first == nil || len(first.Entries) != 1 {
		t.Fatal("大小位置未独立复制", err)
	}
	// 2. 修改返回字段及父目录，再读时取得独立的原字段。
	first.Entries[0].Name, first.Entries[0].FileSize = "本地修改", 0
	*first.Entries[0].ParentID = 999
	second, err := store.ListUserSizeEntryCandidates(context.Background(), 7, sizeEntryCandidateQuery(2), nil)
	if err != nil || second == nil || len(second.Entries) != 1 || !reflect.DeepEqual(second.Entries[0], entry) || *entry.ParentID != 11 {
		t.Fatal("大小候选结果与来源共享字段", err)
	}
}

func TestListUserSizeEntryCandidatesResults(t *testing.T) {
	base := nameEntryCandidateFixture().Entry
	tied, smaller, directory, empty := base, base, base, base
	tied.ID, smaller.ID, smaller.FileSize = 18, 99, 1024
	directory.ID, directory.EntryType, directory.FileSize = 17, "DIR", 0
	empty.ID, empty.FileSize = 15, 0
	repeated := smaller
	repeated.ID = base.ID
	maximum := ^uint64(0)
	large, parent := base, maximum-1
	large.ID, large.UserID, large.FileSize, large.Revision, large.ParentID = maximum, maximum, maximum, maximum, &parent
	var full []domain.FileEntry
	for index := 0; index < 101; index++ {
		entry := base
		entry.ID = uint64(1100 - index)
		full = append(full, entry)
	}
	queryFailure, rowFailure := errors.New("fixture query"), errors.New("fixture rows")
	fixtures := []struct {
		name, scope, kind, search, pattern string
		before                             *domain.SizeEntryPosition
		entries                            []domain.FileEntry
		pageSize                           int
		userID                             uint64
		change                             func([][]driver.Value)
		queryError, rowError, wantError    error
		fail                               bool
	}{
		{name: "empty"},
		{name: "file", entries: []domain.FileEntry{base}},
		{name: "directory", entries: []domain.FileEntry{directory}},
		{name: "empty file", entries: []domain.FileEntry{empty}},
		{name: "same size descending IDs", entries: []domain.FileEntry{base, tied}},
		{name: "size before ID", entries: []domain.FileEntry{base, smaller}},
		{name: "zero sizes", entries: []domain.FileEntry{directory, empty}},
		{name: "extra candidate", entries: []domain.FileEntry{base, tied, smaller}},
		{name: "same size seek", before: &domain.SizeEntryPosition{FileSize: 2048, ID: 20}, entries: []domain.FileEntry{base, tied}},
		{name: "smaller size larger ID", before: &domain.SizeEntryPosition{FileSize: 2048, ID: 1}, entries: []domain.FileEntry{smaller}},
		{name: "zero size seek", before: &domain.SizeEntryPosition{FileSize: 0, ID: 16}, entries: []domain.FileEntry{empty}},
		{name: "zero end", before: &domain.SizeEntryPosition{FileSize: 0, ID: 1}},
		{name: "maximum bound", before: &domain.SizeEntryPosition{FileSize: maximum, ID: maximum}, entries: []domain.FileEntry{base}},
		{name: "maximum values", userID: maximum, entries: []domain.FileEntry{large}},
		{name: "maximum page", pageSize: 100, entries: full},
		{name: "starred", scope: "starred", entries: []domain.FileEntry{base, tied}},
		{name: "starred filtered seek", scope: "starred", kind: "document", search: "B", pattern: "%B%", before: &domain.SizeEntryPosition{FileSize: maximum, ID: maximum}, entries: []domain.FileEntry{base}},
		{name: "folder", kind: "folder", entries: []domain.FileEntry{directory}},
		{name: "document", kind: "document", entries: []domain.FileEntry{base}},
		{name: "image", kind: "image"},
		{name: "video", kind: "video"},
		{name: "audio", kind: "audio"},
		{name: "archive", kind: "archive"},
		{name: "other", kind: "other"},
		{name: "literal percent", search: "%", pattern: "%=%%"},
		{name: "literal underscore", search: "_", pattern: "%=_%"},
		{name: "combined escapes", search: "=%_\\", pattern: "%===%=_\\%"},
		{name: "literal SQL", search: "' OR TRUE -- ?", pattern: "%' OR TRUE -- ?%"},
		{name: "raw whitespace", search: " B ", pattern: "% B %"},
		{name: "raw Unicode", search: "Cafe\u0301", pattern: "%Cafe\u0301%"},
		{name: "embedded zero", search: "\x00", pattern: "%\x00%"},
		{name: "query failure", queryError: queryFailure, wantError: queryFailure, fail: true},
		{name: "partial rows", entries: []domain.FileEntry{base}, rowError: rowFailure, wantError: rowFailure, fail: true},
		{name: "too many rows", entries: []domain.FileEntry{base, tied, smaller, empty}, wantError: domain.ErrInvalidFileEntryData, fail: true},
		{name: "wrong owner", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][1] = "8" }, fail: true},
		{name: "root", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][2] = nil }, fail: true},
		{name: "zero parent", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][2] = "0" }, fail: true},
		{name: "self parent", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][2] = "19" }, fail: true},
		{name: "zero ID", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][0] = "0" }, fail: true},
		{name: "zero revision", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][6] = "0" }, fail: true},
		{name: "inactive", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][7] = "TRASHED" }, fail: true},
		{name: "empty name", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][3] = "" }, fail: true},
		{name: "invalid name", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][3] = string([]byte{0xff}) }, fail: true},
		{name: "invalid type", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][4] = "LINK" }, fail: true},
		{name: "directory size", entries: []domain.FileEntry{directory}, change: func(rows [][]driver.Value) { rows[0][5] = "1" }, fail: true},
		{name: "invalid created", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][8] = time.Time{} }, fail: true},
		{name: "invalid modified", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][9] = time.Time{} }, fail: true},
		{name: "modified precision", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][9] = base.ModifiedAt.Add(time.Nanosecond) }, fail: true},
		{name: "invalid updated", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][10] = time.Time{} }, fail: true},
		{name: "scan size failure", entries: []domain.FileEntry{base}, change: func(rows [][]driver.Value) { rows[0][5] = "not-a-size" }, fail: true},
		{name: "ascending sizes", entries: []domain.FileEntry{smaller, base}, fail: true},
		{name: "ascending tie IDs", entries: []domain.FileEntry{tied, base}, fail: true},
		{name: "duplicate row", entries: []domain.FileEntry{base, base}, fail: true},
		{name: "duplicate ID smaller size", entries: []domain.FileEntry{base, repeated}, fail: true},
		{name: "repeat bound", before: &domain.SizeEntryPosition{FileSize: 2048, ID: 19}, entries: []domain.FileEntry{base}, fail: true},
		{name: "larger than bound", before: &domain.SizeEntryPosition{FileSize: 1024, ID: maximum}, entries: []domain.FileEntry{base}, fail: true},
		{name: "file returned for folder", kind: "folder", entries: []domain.FileEntry{base}, fail: true},
		{name: "directory returned for file", kind: "document", entries: []domain.FileEntry{directory}, fail: true},
		{name: "invalid extra candidate", pageSize: 1, entries: []domain.FileEntry{base, tied}, change: func(rows [][]driver.Value) { rows[1][2] = nil }, fail: true},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 组合预期 SQL 和每项绑定值，包括完整无符号大小及 ID。
			userID, pageSize := fixture.userID, fixture.pageSize
			if userID == 0 {
				userID = 7
			}
			if pageSize == 0 {
				pageSize = 2
			}
			var original domain.SizeEntryPosition
			if fixture.before != nil {
				original = *fixture.before
			}
			var records [][]driver.Value
			for _, entry := range fixture.entries {
				records = append(records, directoryEntryValues(entry))
			}
			if fixture.change != nil {
				fixture.change(records)
			}
			calls := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				calls++
				predicates := []string{"user_id = ? AND status = ? AND parent_id IS NOT NULL"}
				values := []any{userID, "ACTIVE"}
				if fixture.scope == "starred" || fixture.kind != "" || fixture.search != "" || fixture.before != nil {
					predicates[0] = "(" + predicates[0] + ")"
				}
				if fixture.scope == "starred" {
					predicates = append(predicates, "(EXISTS (SELECT 1 FROM user_entry_preferences WHERE user_entry_preferences.user_id = ? AND user_entry_preferences.entry_id = file_entries.id AND user_entry_preferences.starred_at IS NOT NULL))")
					values = append(values, userID)
				}
				if fixture.kind == "folder" {
					predicates = append(predicates, "entry_type = ?")
					values = append(values, "DIR")
				} else if fixture.kind != "" {
					predicates = append(predicates, "(entry_type = ? AND category = ?)")
					values = append(values, "FILE", strings.ToUpper(fixture.kind))
				}
				if fixture.search != "" {
					predicates = append(predicates, "name LIKE ? ESCAPE '='")
					values = append(values, fixture.pattern)
				}
				if fixture.before != nil {
					predicates = append(predicates, "((file_size < ? OR (file_size = ? AND id < ?)))")
					values = append(values, original.FileSize, original.FileSize, original.ID)
				}
				values = append(values, int64(pageSize+1))
				expectedArguments := make([]driver.NamedValue, len(values))
				for index, value := range values {
					expectedArguments[index] = driver.NamedValue{Ordinal: index + 1, Value: value}
				}
				head := "SELECT id,user_id,parent_id,name,entry_type,file_size,revision,status,created_at,modified_at,updated_at FROM file_entries WHERE "
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != head+strings.Join(predicates, " AND ")+" ORDER BY file_size DESC, id DESC LIMIT ?" || !reflect.DeepEqual(arguments, expectedArguments) {
					t.Error("大小候选 SQL 或绑定参数错误", actual)
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("大小候选查询期限错误")
				}
				return &directoryEntryRows{records: records, terminalError: fixture.rowError}, fixture.queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			query := sizeEntryCandidateQuery(pageSize)
			query.Kind, query.Search = fixture.kind, fixture.search
			if fixture.scope != "" {
				query.Scope = fixture.scope
			}
			originalQuery := query
			// 2. 核对完整字段及额外候选，任何失败都返回空结果。
			page, err := store.ListUserSizeEntryCandidates(context.Background(), userID, query, fixture.before)
			if fixture.fail {
				if page != nil || err == nil || (fixture.wantError != nil && !errors.Is(err, fixture.wantError)) {
					t.Fatal("大小候选失败未整体返回空结果", err)
				}
			} else {
				expected := &domain.FileEntryPage{Entries: append([]domain.FileEntry{}, fixture.entries[:min(len(fixture.entries), pageSize)]...), HasMore: len(fixture.entries) > pageSize}
				if err != nil || !reflect.DeepEqual(page, expected) {
					t.Fatal("大小候选字段或分页结果错误", err)
				}
			}
			if calls != 1 || query != originalQuery || (fixture.before != nil && *fixture.before != original) {
				t.Fatal("大小候选调用次数或输入保护错误")
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("大小候选读取关闭了共享连接池", err)
			}
		})
	}
}

func TestListUserSizeEntryCandidatesCancellation(t *testing.T) {
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
					return &directoryEntryRows{records: [][]driver.Value{directoryEntryValues(candidate.Entry)}}, nil
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
			page, err := store.ListUserSizeEntryCandidates(ctx, 7, sizeEntryCandidateQuery(2), nil)
			expected := context.Canceled
			if strings.HasSuffix(mode, "deadline") {
				expected = context.DeadlineExceeded
			}
			if page != nil || !errors.Is(err, expected) {
				t.Fatal("大小候选取消结果或错误链错误", err)
			}
		})
	}
}
