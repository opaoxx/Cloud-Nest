package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUserFileEntriesResults(t *testing.T) {
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	parentID := uint64(11)
	file := domain.FileEntry{ID: 19, UserID: 7, ParentID: &parentID, Name: "学习笔记 🌱.md", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp.Add(time.Second), UpdatedAt: stamp}
	directory := file
	directory.ID, directory.Name, directory.EntryType, directory.FileSize = 18, "课程资料", "DIR", 0
	root := directory
	root.ID, root.ParentID, root.Name = 11, nil, ""
	trashed, purging, purged := directory, directory, file
	trashed.Status, purging.Status, purged.Status = "TRASHED", "PURGING", "PURGED"
	maximum := ^uint64(0)
	maximumParent := maximum - 1
	large := file
	large.ID, large.UserID, large.ParentID, large.FileSize, large.Revision = maximum, maximum, &maximumParent, maximum, maximum
	var fullIDs []uint64
	var repeatedIDs []uint64
	var fullRecords []domain.FileEntry
	for index := range domain.MaxGlobalEntryCandidates {
		entry := directory
		entry.ID = uint64(100 + index)
		fullIDs = append(fullIDs, entry.ID)
		repeatedIDs = append(repeatedIDs, file.ID)
		fullRecords = append(fullRecords, entry)
	}
	queryFailure, rowFailure := errors.New("fixture query failure"), errors.New("fixture partial rows")
	for _, fixture := range []struct {
		name                            string
		ids, sqlIDs                     []uint64
		userID                          uint64
		records                         []domain.FileEntry
		change                          func([][]driver.Value)
		queryError, rowError, wantError error
		failure                         bool
	}{
		{name: "nil batch"},
		{name: "empty batch", ids: []uint64{}},
		{name: "all missing", ids: []uint64{999}},
		{name: "file", ids: []uint64{19}, records: []domain.FileEntry{file}},
		{name: "directory", ids: []uint64{18}, records: []domain.FileEntry{directory}},
		{name: "root", ids: []uint64{11}, records: []domain.FileEntry{root}},
		{name: "unordered rows", ids: []uint64{19, 18, 11}, records: []domain.FileEntry{root, directory, file}},
		{name: "deduplicated ids", ids: []uint64{19, 11, 19, 11}, sqlIDs: []uint64{19, 11}, records: []domain.FileEntry{root, file}},
		{name: "partly missing", ids: []uint64{19, 999}, records: []domain.FileEntry{file}},
		{name: "trashed preserved", ids: []uint64{18}, records: []domain.FileEntry{trashed}},
		{name: "purging preserved", ids: []uint64{18}, records: []domain.FileEntry{purging}},
		{name: "purged preserved", ids: []uint64{19}, records: []domain.FileEntry{purged}},
		{name: "maximum identifiers", ids: []uint64{maximum}, userID: maximum, records: []domain.FileEntry{large}},
		{name: "maximum batch", ids: fullIDs, records: fullRecords},
		{name: "maximum repeated batch", ids: repeatedIDs, sqlIDs: []uint64{19}, records: []domain.FileEntry{file}},
		{name: "query failure", ids: []uint64{19}, queryError: queryFailure, failure: true, wantError: queryFailure},
		{name: "partial row failure", ids: []uint64{19}, records: []domain.FileEntry{file}, rowError: rowFailure, failure: true, wantError: rowFailure},
		{name: "duplicate row", ids: []uint64{19, 19}, sqlIDs: []uint64{19}, records: []domain.FileEntry{file, file}, failure: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "unrequested row", ids: []uint64{18}, records: []domain.FileEntry{file}, failure: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "wrong owner", ids: []uint64{19}, records: []domain.FileEntry{file}, change: func(rows [][]driver.Value) { rows[0][1] = "8" }, failure: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero target", ids: []uint64{19}, records: []domain.FileEntry{file}, change: func(rows [][]driver.Value) { rows[0][0] = "0" }, failure: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero revision", ids: []uint64{19}, records: []domain.FileEntry{file}, change: func(rows [][]driver.Value) { rows[0][6] = "0" }, failure: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "zero parent", ids: []uint64{19}, records: []domain.FileEntry{file}, change: func(rows [][]driver.Value) { rows[0][2] = "0" }, failure: true, wantError: domain.ErrInvalidFileEntryData},
		{name: "invalid id scan", ids: []uint64{19}, records: []domain.FileEntry{file}, change: func(rows [][]driver.Value) { rows[0][0] = "bad-id" }, failure: true},
		{name: "negative parent scan", ids: []uint64{19}, records: []domain.FileEntry{file}, change: func(rows [][]driver.Value) { rows[0][2] = int64(-1) }, failure: true},
		{name: "overflow revision scan", ids: []uint64{19}, records: []domain.FileEntry{file}, change: func(rows [][]driver.Value) { rows[0][6] = "18446744073709551616" }, failure: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 准备结果并核对一条 SELECT 的投影、用户和去重 ID 参数。
			userID := fixture.userID
			if userID == 0 {
				userID = 7
			}
			inputBefore := slices.Clone(fixture.ids)
			expectedIDs := fixture.sqlIDs
			if expectedIDs == nil {
				expectedIDs = fixture.ids
			}
			var rows [][]driver.Value
			for _, entry := range fixture.records {
				rows = append(rows, directoryEntryValues(entry))
			}
			if fixture.change != nil {
				fixture.change(rows)
			}
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				placeholders := strings.TrimSuffix(strings.Repeat("?,", len(expectedIDs)), ",")
				wanted := "SELECT id,user_id,parent_id,name,entry_type,file_size,revision,status,created_at,modified_at,updated_at FROM file_entries WHERE user_id = ? AND id IN (" + placeholders + ")"
				wantedArguments := []driver.NamedValue{{Ordinal: 1, Value: userID}}
				for index, entryID := range expectedIDs {
					wantedArguments = append(wantedArguments, driver.NamedValue{Ordinal: index + 2, Value: entryID})
				}
				if actual != wanted || !reflect.DeepEqual(arguments, wantedArguments) {
					t.Error("批量查询投影、范围、占位符或参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) > 5*time.Second {
					t.Error("批量查询缺少期限")
				}
				return &directoryEntryRows{records: rows, terminalError: fixture.rowError}, fixture.queryError
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 核对完整映射或空结果错误，以及输入和连接池所有权。
			entries, err := store.FindUserFileEntries(context.Background(), userID, fixture.ids)
			if fixture.failure {
				if entries != nil || err == nil || (fixture.wantError != nil && !errors.Is(err, fixture.wantError)) {
					t.Fatal("批量失败返回了部分数据或丢失错误原因", err)
				}
			} else {
				wanted := make(map[uint64]domain.FileEntry)
				for _, entry := range fixture.records {
					wanted[entry.ID] = entry
				}
				if err != nil || entries == nil || !reflect.DeepEqual(entries, wanted) {
					t.Fatal("批量映射、空映射或字段原值错误", err)
				}
				for entryID, entry := range entries {
					if entry.ParentID != nil && entry.ParentID == wanted[entryID].ParentID {
						t.Error("返回父目录指针与输入夹具共享")
					}
				}
			}
			wantQueries := 1
			if len(fixture.ids) == 0 {
				wantQueries = 0
			}
			if queries != wantQueries || !slices.Equal(fixture.ids, inputBefore) {
				t.Fatal("批量查询次数或输入保护错误")
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("批量读取关闭了共享连接池", err)
			}
		})
	}
}

func TestFindUserFileEntriesInputs(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer release()
	tooMany := make([]uint64, domain.MaxGlobalEntryCandidates+1)
	for index := range tooMany {
		tooMany[index] = uint64(index + 1)
	}
	repeated := make([]uint64, domain.MaxGlobalEntryCandidates+1)
	for index := range repeated {
		repeated[index] = 19
	}
	for _, fixture := range []struct {
		name   string
		ctx    context.Context
		userID uint64
		ids    []uint64
		want   error
	}{
		{name: "nil context", userID: 7, ids: []uint64{19}},
		{name: "nil context empty", userID: 7},
		{name: "zero user", ctx: context.Background(), ids: []uint64{19}},
		{name: "zero user empty", ctx: context.Background()},
		{name: "zero id first", ctx: context.Background(), userID: 7, ids: []uint64{0, 19}},
		{name: "zero id last", ctx: context.Background(), userID: 7, ids: []uint64{19, 0}},
		{name: "oversized batch", ctx: context.Background(), userID: 7, ids: tooMany},
		{name: "oversized repeated batch", ctx: context.Background(), userID: 7, ids: repeated},
		{name: "cancelled", ctx: cancelled, userID: 7, ids: []uint64{19}, want: context.Canceled},
		{name: "cancelled empty", ctx: cancelled, userID: 7, want: context.Canceled},
		{name: "expired", ctx: expired, userID: 7, ids: []uint64{19}, want: context.DeadlineExceeded},
		{name: "expired empty", ctx: expired, userID: 7, want: context.DeadlineExceeded},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 错误输入和已取消请求在建立连接前拒绝。
			connector := &deviceConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := store.FindUserFileEntries(fixture.ctx, fixture.userID, fixture.ids)
			if entries != nil || err == nil || (fixture.want != nil && !errors.Is(err, fixture.want)) || connector.connections.Load() != 0 {
				t.Fatal("非法批次建立了连接或取消原因错误", err)
			}
		})
	}
}

func TestFindUserFileEntriesCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after rows", "after missing", "after failure"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 在查询等待或返回时触发取消，核对错误原因优先级。
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
						return &directoryEntryRows{records: [][]driver.Value{{"19", "7", "11", "目录", "DIR", "0", "1", "ACTIVE", time.Now(), time.Now(), time.Now()}}}, nil
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
			entries, err := store.FindUserFileEntries(ctx, 7, []uint64{19})
			want := error(context.DeadlineExceeded)
			if strings.HasPrefix(phase, "after") {
				want = context.Canceled
			}
			if entries != nil || !errors.Is(err, want) {
				t.Fatal("取消后交付了结果或丢失原因", err)
			}
		})
	}
}
