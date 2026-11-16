package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type globalEntryListRequest struct {
	ctx    context.Context
	userID uint64
	query  domain.GlobalEntryQuery
	before *domain.FileEntryPosition
}

type globalEntryListReaderStub struct {
	*visibilityReaderStub
	candidates   []domain.FileEntry
	requests     []globalEntryListRequest
	pages        []*domain.FileEntryPage
	readError    error
	afterRead    func()
	mutateBefore bool
}

func (reader *globalEntryListReaderStub) ListUserGlobalEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.FileEntryPosition) (*domain.FileEntryPage, error) {
	reader.requests = append(reader.requests, globalEntryListRequest{ctx, userID, query, copyOptionalValue(before)})
	var page *domain.FileEntryPage
	if reader.pages != nil {
		index := len(reader.requests) - 1
		if index < len(reader.pages) {
			page = reader.pages[index]
		}
	} else {
		entries := make([]domain.FileEntry, 0)
		for _, entry := range reader.candidates {
			if before == nil || entry.ModifiedAt.Before(before.ModifiedAt) || (entry.ModifiedAt.Equal(before.ModifiedAt) && entry.ID < before.ID) {
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
		before.ID, before.ModifiedAt = 0, time.Time{}
	}
	return page, reader.readError
}

func globalEntryListFixture(count int, hidden func(int) bool) (domain.Actor, *globalEntryListReaderStub, domain.GlobalEntryQuery) {
	actor, visibility, candidates := visibilityFixture()
	parent := *visibility.entries[12]
	parent.ID, parent.Status = 21, "TRASHED"
	visibility.entries[21] = &parent
	reader := &globalEntryListReaderStub{visibilityReaderStub: visibility}
	for index := range count {
		entry := candidates[0]
		entry.ID = uint64(2000 - index)
		entry.ParentID = copyOptionalValue(entry.ParentID)
		if hidden != nil && hidden(index) {
			entry.ParentID = copyOptionalValue(&parent.ID)
		}
		reader.candidates = append(reader.candidates, entry)
	}
	return actor, reader, domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeAll, Sort: domain.GlobalEntrySortModifiedDesc, PageSize: 2}
}

func TestGlobalEntryListServiceResults(t *testing.T) {
	testGlobalEntryListServiceResults(t, domain.GlobalEntryScopeAll)
}

func TestStarredGlobalEntryListServiceResults(t *testing.T) {
	testGlobalEntryListServiceResults(t, domain.GlobalEntryScopeStarred)
}

func testGlobalEntryListServiceResults(t *testing.T, scope string) {
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
			actor, reader, query := globalEntryListFixture(fixture.count, fixture.hidden)
			query.Scope = scope
			if fixture.pageSize != 0 {
				query.PageSize = fixture.pageSize
			}
			query.Search, query.Kind = "笔记%_", "document"
			beforeQuery := query
			ctx := context.WithValue(context.Background(), struct{}{}, "request")
			snapshotContext := context.WithValue(ctx, struct{}{}, "snapshot")
			calls := 0
			listing := NewGlobalEntryListService(func(received context.Context, action func(context.Context, GlobalEntryListReader) error) error {
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
			if (result.NextPosition != nil) != fixture.more || (fixture.more && (result.NextPosition.ID != fixture.nextID || !result.NextPosition.IsValid() || result.NextPosition.ModifiedAt.Location() != time.UTC)) {
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

func TestGlobalEntryListServiceContinuationAndCopies(t *testing.T) {
	testGlobalEntryListServiceContinuationAndCopies(t, domain.GlobalEntryScopeAll)
}

func TestStarredGlobalEntryListServiceContinuationAndCopies(t *testing.T) {
	testGlobalEntryListServiceContinuationAndCopies(t, domain.GlobalEntryScopeStarred)
}

func testGlobalEntryListServiceContinuationAndCopies(t *testing.T, scope string) {
	t.Helper()
	// 1. 页内提前填满后，下一页重新取得未消费的可见项。
	actor, reader, query := globalEntryListFixture(6, func(index int) bool { return index%2 == 0 })
	query.Scope = scope
	listing := NewGlobalEntryListService(func(ctx context.Context, action func(context.Context, GlobalEntryListReader) error) error {
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
	if *reader.candidates[1].ParentID != 12 || reader.candidates[1].Name != "学习笔记.md" {
		t.Fatal("交付结果共享了源记录")
	}
	// 2. 偏移时区位置独立规范为 UTC。
	position.ModifiedAt = position.ModifiedAt.In(time.FixedZone("fixture", 8*3600))
	original := position
	if _, err := listing.List(context.Background(), actor, query, &position); err != nil || position != original || reader.requests[len(reader.requests)-1].before.ModifiedAt.Location() != time.UTC {
		t.Fatal("翻页时间规范化或副本错误", err)
	}
}

func TestGlobalEntryListServiceFailures(t *testing.T) {
	testGlobalEntryListServiceFailures(t, domain.GlobalEntryScopeAll)
}

func TestStarredGlobalEntryListServiceFailures(t *testing.T) {
	testGlobalEntryListServiceFailures(t, domain.GlobalEntryScopeStarred)
}

func testGlobalEntryListServiceFailures(t *testing.T, scope string) {
	t.Helper()
	readFailure, finishFailure := errors.New("fixture read failure"), errors.New("fixture snapshot finish failure")
	for _, mode := range []string{"nil page", "short has more", "too many", "wrong owner", "inactive", "zero id", "zero revision", "nil parent", "zero parent", "empty name", "invalid time", "submicrosecond", "ascending ids", "ascending times", "duplicate id older time", "cross batch duplicate", "read failure", "later read failure", "disabled empty", "snapshot failure", "no callback", "nil snapshot context", "nil reader", "cancelled snapshot", "cancel after read", "cancel after failed read", "cancel after callback"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader, query := globalEntryListFixture(2, nil)
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
				_, many, _ := globalEntryListFixture(101, nil)
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
			case "invalid time":
				reader.candidates[0].ModifiedAt = time.Time{}
			case "submicrosecond":
				reader.candidates[0].ModifiedAt = reader.candidates[0].ModifiedAt.Add(time.Nanosecond)
			case "ascending ids":
				reader.candidates[0], reader.candidates[1] = reader.candidates[1], reader.candidates[0]
			case "ascending times":
				reader.candidates[1].ModifiedAt = reader.candidates[0].ModifiedAt.Add(time.Second)
			case "duplicate id older time":
				reader.candidates[1].ID = reader.candidates[0].ID
				reader.candidates[1].ModifiedAt = reader.candidates[0].ModifiedAt.Add(-time.Second)
			case "read failure":
				reader.readError, want = readFailure, readFailure
			case "cross batch duplicate":
				_, many, _ := globalEntryListFixture(100, func(int) bool { return true })
				repeated := many.candidates[0]
				repeated.ModifiedAt = repeated.ModifiedAt.Add(-time.Second)
				reader.pages = []*domain.FileEntryPage{{Entries: many.candidates, HasMore: true}, {Entries: []domain.FileEntry{repeated}}}
			case "later read failure":
				_, many, _ := globalEntryListFixture(101, func(index int) bool { return index != 0 })
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
			listing := NewGlobalEntryListService(func(current context.Context, action func(context.Context, GlobalEntryListReader) error) error {
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

func TestGlobalEntryListServiceInputs(t *testing.T) {
	testGlobalEntryListServiceInputs(t, domain.GlobalEntryScopeAll)
}

func TestStarredGlobalEntryListServiceInputs(t *testing.T) {
	testGlobalEntryListServiceInputs(t, domain.GlobalEntryScopeStarred)
}

func testGlobalEntryListServiceInputs(t *testing.T, scope string) {
	t.Helper()
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "invalid query", "recent scope", "starred name sort", "name sort", "zero position id", "zero position time", "position precision", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			actor, _, query := globalEntryListFixture(0, nil)
			query.Scope = scope
			var ctx context.Context = context.Background()
			var before *domain.FileEntryPosition
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
				before = &domain.FileEntryPosition{ModifiedAt: time.Now().UTC().Truncate(time.Microsecond)}
			case "zero position time":
				before = &domain.FileEntryPosition{ID: 10}
			case "position precision":
				before = &domain.FileEntryPosition{ID: 10, ModifiedAt: time.Now().UTC().Truncate(time.Microsecond).Add(time.Nanosecond)}
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
			listing := NewGlobalEntryListService(func(context.Context, func(context.Context, GlobalEntryListReader) error) error { calls++; return nil })
			result, err := listing.List(ctx, actor, query, before)
			if result != nil || err == nil || calls != 0 || (want != nil && !errors.Is(err, want)) {
				t.Fatal("错误输入进入快照或取消原因不符", err)
			}
		})
	}
}

func TestGlobalEntryListServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少快照依赖未拒绝")
		}
	}()
	NewGlobalEntryListService(nil)
}
