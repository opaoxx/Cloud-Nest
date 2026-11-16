package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type sizeEntryListRequest struct {
	ctx    context.Context
	userID uint64
	query  domain.GlobalEntryQuery
	before *domain.SizeEntryPosition
}

type sizeEntryListReaderStub struct {
	*visibilityReaderStub
	candidates   []domain.FileEntry
	requests     []sizeEntryListRequest
	pages        []*domain.FileEntryPage
	readError    error
	afterRead    func()
	mutateBefore bool
}

func (reader *sizeEntryListReaderStub) ListUserSizeEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.SizeEntryPosition) (*domain.FileEntryPage, error) {
	reader.requests = append(reader.requests, sizeEntryListRequest{ctx, userID, query, copyOptionalValue(before)})
	var page *domain.FileEntryPage
	if reader.pages != nil {
		index := len(reader.requests) - 1
		if index < len(reader.pages) {
			page = reader.pages[index]
		}
	} else {
		entries := make([]domain.FileEntry, 0)
		for _, entry := range reader.candidates {
			if before == nil || entry.FileSize < before.FileSize || (entry.FileSize == before.FileSize && entry.ID < before.ID) {
				entries = append(entries, entry)
			}
		}
		more := len(entries) > query.PageSize
		if more {
			entries = entries[:query.PageSize]
		}
		page = &domain.FileEntryPage{Entries: entries, HasMore: more}
	}
	if reader.afterRead != nil {
		reader.afterRead()
	}
	if reader.mutateBefore && before != nil {
		before.ID, before.FileSize = 0, 0
	}
	return page, reader.readError
}

func sizeEntryListFixture(count int, hidden func(int) bool) (domain.Actor, *sizeEntryListReaderStub, domain.GlobalEntryQuery) {
	actor, visibility, candidates := visibilityFixture()
	parent := *visibility.entries[12]
	parent.ID, parent.Status = 21, "TRASHED"
	visibility.entries[21] = &parent
	reader := &sizeEntryListReaderStub{visibilityReaderStub: visibility}
	for index := range count {
		entry := candidates[0]
		entry.ID = uint64(2000 - index)
		entry.ParentID = copyOptionalValue(entry.ParentID)
		if hidden != nil && hidden(index) {
			entry.ParentID = copyOptionalValue(&parent.ID)
		}
		reader.candidates = append(reader.candidates, entry)
	}
	return actor, reader, domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeAll, Sort: domain.GlobalEntrySortSizeDesc, PageSize: 2}
}

func TestSizeEntryListServiceResults(t *testing.T) {
	testSizeEntryListServiceResults(t, domain.GlobalEntryScopeAll)
}

func TestStarredSizeEntryListServiceResults(t *testing.T) {
	testSizeEntryListServiceResults(t, domain.GlobalEntryScopeStarred)
}

func testSizeEntryListServiceResults(t *testing.T, scope string) {
	t.Helper()
	for _, fixture := range []struct {
		name      string
		count     int
		pageSize  int
		hidden    func(int) bool
		ids       []uint64
		more      bool
		nextID    uint64
		readCount int
	}{
		{name: "empty", readCount: 1},
		{name: "single", count: 1, ids: []uint64{2000}, readCount: 1},
		{name: "exact page", count: 2, ids: []uint64{2000, 1999}, readCount: 1},
		{name: "unconsumed batch tail", count: 3, ids: []uint64{2000, 1999}, more: true, nextID: 1999, readCount: 1},
		{name: "mixed prefix", count: 6, hidden: func(index int) bool { return index%2 == 0 }, ids: []uint64{1999, 1997}, more: true, nextID: 1997, readCount: 1},
		{name: "hidden short end", count: 10, hidden: func(int) bool { return true }, readCount: 1},
		{name: "refill next batch", count: 102, hidden: func(index int) bool { return index < 100 }, ids: []uint64{1900, 1899}, readCount: 2},
		{name: "short visible end", count: 100, hidden: func(index int) bool { return index < 99 }, ids: []uint64{1901}, readCount: 1},
		{name: "budget empty continuation", count: 1001, hidden: func(int) bool { return true }, more: true, nextID: 1001, readCount: 10},
		{name: "budget exact end", count: 1000, hidden: func(int) bool { return true }, readCount: 10},
		{name: "budget short continuation", count: 1001, pageSize: 3, hidden: func(index int) bool { return index != 0 && index != 998 && index != 1000 }, ids: []uint64{2000, 1002}, more: true, nextID: 1001, readCount: 10},
		{name: "full at budget edge", count: 1001, pageSize: 1, hidden: func(index int) bool { return index != 999 }, ids: []uint64{1001}, more: true, nextID: 1001, readCount: 10},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 组合一次快照中的候选与真实可见性函数。
			actor, reader, query := sizeEntryListFixture(fixture.count, fixture.hidden)
			query.Scope = scope
			if fixture.pageSize != 0 {
				query.PageSize = fixture.pageSize
			}
			query.Search, query.Kind = "笔记%_", "document"
			beforeQuery := query
			ctx := context.WithValue(context.Background(), struct{}{}, "request")
			snapshotContext := context.WithValue(ctx, struct{}{}, "snapshot")
			calls := 0
			listing := NewSizeEntryListService(func(received context.Context, action func(context.Context, SizeEntryListReader) error) error {
				calls++
				if received != ctx {
					t.Error("快照入口上下文错误")
				}
				return action(snapshotContext, reader)
			})
			result, err := listing.List(ctx, actor, query, nil)
			if err != nil || result == nil || result.Entries == nil || len(result.Entries) != len(fixture.ids) || result.HasMore != fixture.more || calls != 1 || len(reader.requests) != fixture.readCount || query != beforeQuery {
				t.Fatal("列表、快照、补读或查询原值错误", err)
			}
			for index, entryID := range fixture.ids {
				if result.Entries[index].ID != entryID || !reflect.DeepEqual(result.Entries[index], reader.candidates[2000-entryID]) {
					t.Fatal("可见结果顺序或基础字段错误")
				}
			}
			if (result.NextPosition != nil) != fixture.more || (fixture.more && (result.NextPosition.ID != fixture.nextID || !result.NextPosition.IsValid() || result.NextPosition.FileSize != 2048)) {
				t.Fatal("最后消费位置或后续页提示错误")
			}
			for _, request := range reader.requests {
				expected := query
				expected.PageSize = 100
				if request.ctx != snapshotContext || request.userID != actor.UserID || request.query != expected {
					t.Fatal("候选批次上下文、用户或筛选参数错误")
				}
			}
		})
	}
}

func TestSizeEntryListServiceContinuationAndCopies(t *testing.T) {
	testSizeEntryListServiceContinuationAndCopies(t, domain.GlobalEntryScopeAll)
}

func TestStarredSizeEntryListServiceContinuationAndCopies(t *testing.T) {
	testSizeEntryListServiceContinuationAndCopies(t, domain.GlobalEntryScopeStarred)
}

func testSizeEntryListServiceContinuationAndCopies(t *testing.T, scope string) {
	t.Helper()
	// 1. 页内提前填满后，下一页重新取得未消费的可见项。
	actor, reader, query := sizeEntryListFixture(6, func(index int) bool { return index%2 == 0 })
	query.Scope = scope
	listing := NewSizeEntryListService(func(ctx context.Context, action func(context.Context, SizeEntryListReader) error) error {
		return action(ctx, reader)
	})
	first, err := listing.List(context.Background(), actor, query, nil)
	if err != nil || first == nil || first.NextPosition == nil {
		t.Fatal(err)
	}
	position := *first.NextPosition
	reader.mutateBefore = true
	second, err := listing.List(context.Background(), actor, query, &position)
	if err != nil || len(second.Entries) != 1 || second.Entries[0].ID != 1995 || second.HasMore || second.NextPosition != nil || position != *first.NextPosition {
		t.Fatal("续页丢失批次余项或修改输入位置", err)
	}
	*first.Entries[0].ParentID = 999
	first.Entries[0].Name = "本地副本"
	first.Entries[0].FileSize = 0
	if *reader.candidates[1].ParentID != 12 || reader.candidates[1].Name != "学习笔记.md" || reader.candidates[1].FileSize != 2048 {
		t.Fatal("交付结果共享了源记录")
	}
	// 2. 位置值保持独立。
	original := position
	if _, err := listing.List(context.Background(), actor, query, &position); err != nil || position != original || reader.requests[len(reader.requests)-1].before.FileSize != original.FileSize {
		t.Fatal("大小翻页位置或副本错误", err)
	}
}

func TestSizeEntryListServiceFailures(t *testing.T) {
	testSizeEntryListServiceFailures(t, domain.GlobalEntryScopeAll)
}

func TestStarredSizeEntryListServiceFailures(t *testing.T) {
	testSizeEntryListServiceFailures(t, domain.GlobalEntryScopeStarred)
}

func testSizeEntryListServiceFailures(t *testing.T, scope string) {
	t.Helper()
	readFailure, finishFailure := errors.New("fixture read failure"), errors.New("fixture snapshot finish failure")
	for _, mode := range []string{"nil page", "short has more", "too many", "wrong owner", "inactive", "zero id", "zero revision", "nil parent", "zero parent", "empty name", "invalid time", "submicrosecond", "ascending ids", "ascending sizes", "duplicate ID smaller size", "cross batch duplicate", "read failure", "later read failure", "disabled empty", "snapshot failure", "no callback", "nil snapshot context", "nil reader", "cancelled snapshot", "cancel after read", "cancel after failed read", "cancel after callback", "self parent", "invalid name", "invalid type", "directory size"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader, query := sizeEntryListFixture(2, nil)
			query.Scope = scope
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(domain.ErrInvalidFileEntryData)
			switch mode {
			case "nil page":
				reader.pages = []*domain.FileEntryPage{nil}
			case "short has more":
				reader.pages = []*domain.FileEntryPage{{Entries: reader.candidates[:1], HasMore: true}}
			case "too many":
				_, many, _ := sizeEntryListFixture(101, nil)
				reader.pages = []*domain.FileEntryPage{{Entries: many.candidates}}
			case "wrong owner":
				reader.candidates[0].UserID = 8
			case "inactive":
				reader.candidates[0].Status = "TRASHED"
			case "zero id":
				reader.candidates[0].ID = 0
			case "zero revision":
				reader.candidates[0].Revision = 0
			case "nil parent":
				reader.candidates[0].ParentID = nil
			case "zero parent":
				reader.candidates[0].ParentID = new(uint64)
			case "empty name":
				reader.candidates[0].Name = ""
			case "self parent":
				reader.candidates[0].ParentID = &reader.candidates[0].ID
			case "invalid name":
				reader.candidates[0].Name = string([]byte{0xff})
			case "invalid type":
				reader.candidates[0].EntryType = "LINK"
			case "directory size":
				reader.candidates[0].EntryType = "DIR"
			case "invalid time":
				reader.candidates[0].ModifiedAt = time.Time{}
			case "submicrosecond":
				reader.candidates[0].ModifiedAt = reader.candidates[0].ModifiedAt.Add(time.Nanosecond)
			case "ascending ids":
				reader.candidates[0], reader.candidates[1] = reader.candidates[1], reader.candidates[0]
			case "ascending sizes":
				reader.candidates[1].FileSize = reader.candidates[0].FileSize + 1
			case "duplicate ID smaller size":
				reader.candidates[1].ID = reader.candidates[0].ID
				reader.candidates[1].FileSize = reader.candidates[0].FileSize - 1
			case "read failure":
				reader.readError, want = readFailure, readFailure
			case "cross batch duplicate":
				_, many, _ := sizeEntryListFixture(100, func(int) bool { return true })
				repeated := many.candidates[0]
				repeated.FileSize--
				reader.pages = []*domain.FileEntryPage{{Entries: many.candidates, HasMore: true}, {Entries: []domain.FileEntry{repeated}}}
			case "later read failure":
				_, many, _ := sizeEntryListFixture(101, func(index int) bool { return index != 0 })
				reader.candidates = many.candidates
				reader.afterRead = func() {
					if len(reader.requests) == 2 {
						reader.readError = readFailure
					}
				}
				want = readFailure
			case "disabled empty":
				reader.candidates, reader.profile.Status, want = nil, "DISABLED", domain.ErrAccountDisabled
			case "snapshot failure":
				want = finishFailure
			case "nil snapshot context", "nil reader":
				want = nil
			case "cancel after read":
				reader.afterRead, want = cancel, context.Canceled
			case "cancel after failed read":
				reader.afterRead, reader.readError, want = cancel, readFailure, context.Canceled
			case "cancel after callback":
				want = context.Canceled
			case "cancelled snapshot":
				want = context.Canceled
			}
			listing := NewSizeEntryListService(func(current context.Context, action func(context.Context, SizeEntryListReader) error) error {
				if mode == "no callback" {
					return nil
				}
				if mode == "nil snapshot context" {
					return action(nil, reader)
				}
				if mode == "nil reader" {
					return action(current, nil)
				}
				if mode == "cancelled snapshot" {
					cancelled, stop := context.WithCancel(current)
					stop()
					return action(cancelled, reader)
				}
				if err := action(current, reader); err != nil {
					return err
				}
				if mode == "cancel after callback" {
					cancel()
				}
				if mode == "snapshot failure" {
					return finishFailure
				}
				return nil
			})
			result, err := listing.List(ctx, actor, query, nil)
			if result != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("异常页、快照或取消没有整体失败", err)
			}
		})
	}
}

func TestSizeEntryListServiceInputs(t *testing.T) {
	testSizeEntryListServiceInputs(t, domain.GlobalEntryScopeAll)
}

func TestStarredSizeEntryListServiceInputs(t *testing.T) {
	testSizeEntryListServiceInputs(t, domain.GlobalEntryScopeStarred)
}

func testSizeEntryListServiceInputs(t *testing.T, scope string) {
	t.Helper()
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "invalid query", "recent scope", "starred name sort", "name sort", "zero position id", "directory scope", "large page", "modified sort", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			actor, _, query := sizeEntryListFixture(0, nil)
			query.Scope = scope
			var ctx context.Context = context.Background()
			var before *domain.SizeEntryPosition
			var want error
			switch mode {
			case "nil context":
				ctx = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "invalid query":
				query.PageSize = 0
			case "recent scope":
				query.Scope, query.Sort = domain.GlobalEntryScopeRecent, domain.GlobalEntrySortLastOpenedDesc
			case "starred name sort":
				query.Scope, query.Sort = domain.GlobalEntryScopeStarred, domain.GlobalEntrySortNameAsc
			case "name sort":
				query.Sort = domain.GlobalEntrySortNameAsc
			case "zero position id":
				before = &domain.SizeEntryPosition{FileSize: 2048}
			case "directory scope":
				query.Scope = "directory"
			case "large page":
				query.PageSize = 101
			case "modified sort":
				query.Sort = domain.GlobalEntrySortModifiedDesc
			case "cancelled":
				current, cancel := context.WithCancel(ctx)
				cancel()
				ctx, want = current, context.Canceled
			case "expired":
				current, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				ctx, want = current, context.DeadlineExceeded
			}
			calls := 0
			listing := NewSizeEntryListService(func(context.Context, func(context.Context, SizeEntryListReader) error) error { calls++; return nil })
			result, err := listing.List(ctx, actor, query, before)
			if result != nil || err == nil || calls != 0 || (want != nil && !errors.Is(err, want)) {
				t.Fatal("错误输入进入快照或取消原因不符", err)
			}
		})
	}
}

func TestSizeEntryListServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少快照依赖未拒绝")
		}
	}()
	NewSizeEntryListService(nil)
}

func TestSizeEntryListServiceSnapshotCopies(t *testing.T) {
	// 1. 祖先读取期间修改源候选，核对已复制的记录及大小位置。
	actor, reader, query := sizeEntryListFixture(2, nil)
	query.PageSize = 1
	original := reader.candidates[0]
	original.ParentID = copyOptionalValue(original.ParentID)
	reader.afterBatch = func() {
		*reader.candidates[0].ParentID = 999
		reader.candidates[0].Name, reader.candidates[0].FileSize = "后续源修改", 0
	}
	listing := NewSizeEntryListService(func(ctx context.Context, action func(context.Context, SizeEntryListReader) error) error {
		return action(ctx, reader)
	})
	result, err := listing.List(context.Background(), actor, query, nil)
	if err != nil || result == nil || len(result.Entries) != 1 || !reflect.DeepEqual(result.Entries[0], original) || result.NextPosition == nil || *result.NextPosition != (domain.SizeEntryPosition{FileSize: original.FileSize, ID: original.ID}) {
		t.Fatal("祖先读取期间的源变化影响大小列表或位置", err)
	}
}

func TestSizeEntryListServicePositionBounds(t *testing.T) {
	for _, mode := range []string{"same size smaller IDs", "smaller size larger IDs", "zero sizes", "directories", "maximum size and IDs", "sizes override IDs", "repeated bound", "larger tie ID", "larger size"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader, query := sizeEntryListFixture(2, nil)
			before := domain.SizeEntryPosition{FileSize: 2048, ID: 2001}
			valid := true
			switch mode {
			case "smaller size larger IDs":
				before.FileSize, before.ID = 4096, 1
			case "zero sizes", "directories":
				before.FileSize = 0
				for index := range reader.candidates {
					reader.candidates[index].FileSize = 0
					if mode == "directories" {
						reader.candidates[index].EntryType = "DIR"
					}
				}
			case "maximum size and IDs":
				before.FileSize, before.ID = ^uint64(0), ^uint64(0)
				for index := range reader.candidates {
					reader.candidates[index].FileSize, reader.candidates[index].ID = ^uint64(0), ^uint64(0)-uint64(index)-1
				}
			case "sizes override IDs":
				before.FileSize, before.ID = 8192, 1
				reader.candidates[0].FileSize, reader.candidates[0].ID = 4096, 1
				reader.candidates[1].FileSize, reader.candidates[1].ID = 2048, ^uint64(0)
			case "repeated bound":
				before.ID, valid = 2000, false
			case "larger tie ID":
				before.ID, valid = 1999, false
			case "larger size":
				before.FileSize, valid = 1024, false
			}
			original := before
			reader.pages = []*domain.FileEntryPage{{Entries: reader.candidates}}
			listing := NewSizeEntryListService(func(ctx context.Context, action func(context.Context, SizeEntryListReader) error) error {
				return action(ctx, reader)
			})
			result, err := listing.List(context.Background(), actor, query, &before)
			if before != original {
				t.Fatal("大小位置边界被修改")
			}
			if valid {
				if err != nil || result == nil || len(result.Entries) != 2 || result.HasMore || result.NextPosition != nil || !reflect.DeepEqual(result.Entries, reader.candidates) {
					t.Fatal("合法大小位置或基础事实被改变", err)
				}
			} else if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("大小位置边界异常未拒绝", err)
			}
		})
	}
}
