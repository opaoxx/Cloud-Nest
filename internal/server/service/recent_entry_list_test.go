package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type recentEntryListRequest struct {
	ctx    context.Context
	userID uint64
	query  domain.GlobalEntryQuery
	before *domain.RecentEntryPosition
}

type recentEntryListReaderStub struct {
	*visibilityReaderStub
	candidates   []domain.RecentEntryCandidate
	requests     []recentEntryListRequest
	pages        []*domain.RecentEntryPage
	readError    error
	afterRead    func()
	mutateBefore bool
}

func (reader *recentEntryListReaderStub) ListUserRecentEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.RecentEntryPosition) (*domain.RecentEntryPage, error) {
	reader.requests = append(reader.requests, recentEntryListRequest{ctx, userID, query, copyOptionalValue(before)})
	var page *domain.RecentEntryPage
	if reader.pages != nil {
		index := len(reader.requests) - 1
		if index < len(reader.pages) {
			page = reader.pages[index]
		}
	} else {
		entries := make([]domain.RecentEntryCandidate, 0)
		for _, candidate := range reader.candidates {
			if before == nil || candidate.LastOpenedAt.Before(before.LastOpenedAt) || (candidate.LastOpenedAt.Equal(before.LastOpenedAt) && candidate.Entry.ID < before.ID) {
				entries = append(entries, candidate)
			}
		}
		more := len(entries) > query.PageSize
		if more {
			entries = entries[:query.PageSize]
		}
		page = &domain.RecentEntryPage{Entries: entries, HasMore: more}
	}
	if reader.afterRead != nil {
		reader.afterRead()
	}
	if reader.mutateBefore && before != nil {
		before.ID, before.LastOpenedAt = 0, time.Time{}
	}
	return page, reader.readError
}

func recentEntryListFixture(count int, hidden func(int) bool) (domain.Actor, *recentEntryListReaderStub, domain.GlobalEntryQuery) {
	// 1. 复用已有完整父链夹具，给每个候选附加独立访问事实。
	actor, baseReader, query := globalEntryListFixture(count, hidden)
	reader := &recentEntryListReaderStub{visibilityReaderStub: baseReader.visibilityReaderStub}
	for _, entry := range baseReader.candidates {
		reader.candidates = append(reader.candidates, domain.RecentEntryCandidate{Entry: entry, LastOpenedAt: entry.ModifiedAt.Add(time.Hour)})
	}
	query.Scope, query.Sort = domain.GlobalEntryScopeRecent, domain.GlobalEntrySortLastOpenedDesc
	return actor, reader, query
}

func TestRecentEntryListServiceResults(t *testing.T) {
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
		{name: "mixed hidden", count: 6, hidden: func(index int) bool { return index%2 == 0 }, ids: []uint64{1999, 1997}, more: true, nextID: 1997, readCount: 1},
		{name: "hidden end", count: 10, hidden: func(int) bool { return true }, readCount: 1},
		{name: "refill next batch", count: 102, hidden: func(index int) bool { return index < 100 }, ids: []uint64{1900, 1899}, readCount: 2},
		{name: "short visible end", count: 100, hidden: func(index int) bool { return index < 99 }, ids: []uint64{1901}, readCount: 1},
		{name: "budget empty continuation", count: 1001, hidden: func(int) bool { return true }, more: true, nextID: 1001, readCount: 10},
		{name: "budget exact end", count: 1000, hidden: func(int) bool { return true }, readCount: 10},
		{name: "budget short continuation", count: 1001, pageSize: 3, hidden: func(index int) bool { return index != 0 && index != 998 && index != 1000 }, ids: []uint64{2000, 1002}, more: true, nextID: 1001, readCount: 10},
		{name: "full at budget edge", count: 1001, pageSize: 1, hidden: func(index int) bool { return index != 999 }, ids: []uint64{1001}, more: true, nextID: 1001, readCount: 10},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 组合真实可见性函数，核对访问事实和每批原查询条件。
			actor, reader, query := recentEntryListFixture(fixture.count, fixture.hidden)
			if fixture.pageSize != 0 {
				query.PageSize = fixture.pageSize
			}
			query.Search, query.Kind = "笔记%_", "document"
			original := query
			ctx := context.WithValue(context.Background(), struct{}{}, "request")
			snapshotContext := context.WithValue(ctx, struct{}{}, "snapshot")
			calls := 0
			listing := NewRecentEntryListService(func(received context.Context, action func(context.Context, RecentEntryListReader) error) error {
				calls++
				if received != ctx {
					t.Error("最近列表快照上下文错误")
				}
				return action(snapshotContext, reader)
			})
			result, err := listing.List(ctx, actor, query, nil)
			if err != nil || result == nil || result.Entries == nil || len(result.Entries) != len(fixture.ids) || result.HasMore != fixture.more || calls != 1 || len(reader.requests) != fixture.readCount || query != original {
				t.Fatal("最近列表、补读或快照错误", err)
			}
			for index, entryID := range fixture.ids {
				if result.Entries[index].Entry.ID != entryID || !reflect.DeepEqual(result.Entries[index], reader.candidates[2000-entryID]) {
					t.Fatal("可见文件顺序或访问时间与原候选不符")
				}
			}
			if (result.NextPosition != nil) != fixture.more || (fixture.more && (result.NextPosition.ID != fixture.nextID || !result.NextPosition.IsValid() || result.NextPosition.LastOpenedAt.Location() != time.UTC || !result.NextPosition.LastOpenedAt.Equal(reader.candidates[2000-fixture.nextID].LastOpenedAt))) {
				t.Fatal("最近列表最后消费位置错误")
			}
			for _, request := range reader.requests {
				expected := query
				expected.PageSize = 100
				if request.ctx != snapshotContext || request.userID != actor.UserID || request.query != expected {
					t.Fatal("最近候选批次丢失身份、上下文或筛选条件")
				}
			}
		})
	}
}

func TestRecentEntryListServiceContinuationAndCopies(t *testing.T) {
	// 1. 页满后重新读取未消费尾部，驱动修改输入位置时仍保护调用方。
	actor, reader, query := recentEntryListFixture(6, func(index int) bool { return index%2 == 0 })
	listing := NewRecentEntryListService(func(ctx context.Context, action func(context.Context, RecentEntryListReader) error) error {
		return action(ctx, reader)
	})
	first, err := listing.List(context.Background(), actor, query, nil)
	if err != nil || first == nil || first.NextPosition == nil {
		t.Fatal(err)
	}
	position := *first.NextPosition
	reader.mutateBefore = true
	second, err := listing.List(context.Background(), actor, query, &position)
	if err != nil || second == nil || len(second.Entries) != 1 || second.Entries[0].Entry.ID != 1995 || second.HasMore || second.NextPosition != nil || position != *first.NextPosition {
		t.Fatal("最近续页丢失候选或修改了输入位置", err)
	}
	*first.Entries[0].Entry.ParentID = 999
	first.Entries[0].Entry.Name = "本地副本"
	first.Entries[0].LastOpenedAt = time.Time{}
	if *reader.candidates[1].Entry.ParentID != 12 || reader.candidates[1].Entry.Name != "学习笔记.md" || reader.candidates[1].LastOpenedAt.IsZero() {
		t.Fatal("交付结果共享源记录")
	}
	// 2. 在位置副本上换算 UTC，并核对源访问时间精度。
	position.LastOpenedAt = position.LastOpenedAt.In(time.FixedZone("fixture", 8*3600))
	original := position
	if _, err := listing.List(context.Background(), actor, query, &position); err != nil || position != original || reader.requests[len(reader.requests)-1].before.LastOpenedAt.Location() != time.UTC {
		t.Fatal("最近访问位置换算或输入保护错误", err)
	}
}

func TestRecentEntryListServiceFailures(t *testing.T) {
	readFailure, finishFailure := errors.New("fixture read failure"), errors.New("fixture snapshot failure")
	for _, mode := range []string{"nil page", "short has more", "too many", "wrong owner", "inactive", "directory", "folder candidate", "zero id", "zero revision", "nil parent", "zero parent", "empty name", "invalid modified time", "invalid access time", "access precision", "ascending ids", "ascending access times", "duplicate id older access", "cross batch duplicate", "read failure", "later read failure", "visibility failure", "disabled empty", "snapshot failure", "no callback", "nil snapshot context", "nil reader", "cancelled snapshot", "cancel after read", "cancel after failed read", "cancel during ancestors", "cancel after callback"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader, query := recentEntryListFixture(2, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(domain.ErrInvalidFileEntryData)
			switch mode {
			case "nil page":
				reader.pages = []*domain.RecentEntryPage{nil}
			case "short has more":
				reader.pages = []*domain.RecentEntryPage{{Entries: reader.candidates[:1], HasMore: true}}
			case "too many":
				_, many, _ := recentEntryListFixture(101, nil)
				reader.pages = []*domain.RecentEntryPage{{Entries: many.candidates}}
			case "wrong owner":
				reader.candidates[0].Entry.UserID = 8
			case "inactive":
				reader.candidates[0].Entry.Status = "TRASHED"
			case "directory":
				reader.candidates[0].Entry.EntryType = "DIR"
			case "folder candidate":
				query.Kind = "folder"
			case "zero id":
				reader.candidates[0].Entry.ID = 0
			case "zero revision":
				reader.candidates[0].Entry.Revision = 0
			case "nil parent":
				reader.candidates[0].Entry.ParentID = nil
			case "zero parent":
				reader.candidates[0].Entry.ParentID = new(uint64)
			case "empty name":
				reader.candidates[0].Entry.Name = ""
			case "invalid modified time":
				reader.candidates[0].Entry.ModifiedAt = time.Time{}
			case "invalid access time":
				reader.candidates[0].LastOpenedAt = time.Time{}
			case "access precision":
				reader.candidates[0].LastOpenedAt = reader.candidates[0].LastOpenedAt.Add(time.Nanosecond)
			case "ascending ids":
				reader.candidates[0], reader.candidates[1] = reader.candidates[1], reader.candidates[0]
			case "ascending access times":
				reader.candidates[1].LastOpenedAt = reader.candidates[0].LastOpenedAt.Add(time.Second)
			case "duplicate id older access":
				reader.candidates[1].Entry.ID = reader.candidates[0].Entry.ID
				reader.candidates[1].LastOpenedAt = reader.candidates[0].LastOpenedAt.Add(-time.Second)
			case "cross batch duplicate":
				_, many, _ := recentEntryListFixture(100, func(int) bool { return true })
				repeated := many.candidates[0]
				repeated.LastOpenedAt = repeated.LastOpenedAt.Add(-time.Second)
				reader.pages = []*domain.RecentEntryPage{{Entries: many.candidates, HasMore: true}, {Entries: []domain.RecentEntryCandidate{repeated}}}
			case "read failure":
				reader.readError, want = readFailure, readFailure
			case "later read failure":
				_, many, _ := recentEntryListFixture(101, func(index int) bool { return index != 0 })
				reader.candidates = many.candidates
				reader.afterRead = func() {
					if len(reader.requests) == 2 {
						reader.readError = readFailure
					}
				}
				want = readFailure
			case "visibility failure":
				reader.batchError, want = readFailure, readFailure
			case "disabled empty":
				reader.candidates, reader.profile.Status, want = nil, "DISABLED", domain.ErrAccountDisabled
			case "snapshot failure":
				want = finishFailure
			case "nil snapshot context", "nil reader":
				want = nil
			case "cancel after read", "cancel after failed read":
				reader.afterRead, want = cancel, context.Canceled
				if mode == "cancel after failed read" {
					reader.readError = readFailure
				}
			case "cancel during ancestors":
				reader.afterBatch, want = cancel, context.Canceled
			case "cancel after callback", "cancelled snapshot":
				want = context.Canceled
			}
			listing := NewRecentEntryListService(func(current context.Context, action func(context.Context, RecentEntryListReader) error) error {
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
				t.Fatal("最近列表异常或取消没有整体失败", err)
			}
		})
	}
}

func TestRecentEntryListServiceInputs(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "invalid query", "all scope", "starred scope", "modified sort", "invalid UTF-8", "invalid kind", "zero position id", "zero position time", "position precision", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			actor, _, query := recentEntryListFixture(0, nil)
			var ctx context.Context = context.Background()
			var before *domain.RecentEntryPosition
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
			case "all scope", "starred scope":
				query.Scope = domain.GlobalEntryScopeAll
				if mode == "starred scope" {
					query.Scope = domain.GlobalEntryScopeStarred
				}
				query.Sort = domain.GlobalEntrySortModifiedDesc
			case "modified sort":
				query.Sort = domain.GlobalEntrySortModifiedDesc
			case "invalid UTF-8":
				query.Search = string([]byte{0xff})
			case "invalid kind":
				query.Kind = "private-kind"
			case "zero position id":
				before = &domain.RecentEntryPosition{LastOpenedAt: time.Now().UTC().Truncate(time.Microsecond)}
			case "zero position time":
				before = &domain.RecentEntryPosition{ID: 19}
			case "position precision":
				before = &domain.RecentEntryPosition{ID: 19, LastOpenedAt: time.Now().UTC().Truncate(time.Microsecond).Add(time.Nanosecond)}
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
			listing := NewRecentEntryListService(func(context.Context, func(context.Context, RecentEntryListReader) error) error { calls++; return nil })
			result, err := listing.List(ctx, actor, query, before)
			if result != nil || err == nil || calls != 0 || (want != nil && !errors.Is(err, want)) {
				t.Fatal("错误输入进入最近列表快照或取消原因错误", err)
			}
		})
	}
}

func TestRecentEntryListServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少最近列表快照依赖未拒绝")
		}
	}()
	NewRecentEntryListService(nil)
}

func TestRecentEntryListServiceSnapshotCopies(t *testing.T) {
	// 1. 在祖先读取期间修改源候选，验证文件和访问事实已独立复制。
	actor, reader, query := recentEntryListFixture(2, nil)
	query.PageSize = 1
	original := reader.candidates[0]
	original.Entry.ParentID = copyOptionalValue(original.Entry.ParentID)
	reader.afterBatch = func() {
		*reader.candidates[0].Entry.ParentID = 999
		reader.candidates[0].Entry.Name = "后续源修改"
		reader.candidates[0].LastOpenedAt = original.LastOpenedAt.Add(time.Hour)
	}
	listing := NewRecentEntryListService(func(ctx context.Context, action func(context.Context, RecentEntryListReader) error) error {
		return action(ctx, reader)
	})
	result, err := listing.List(context.Background(), actor, query, nil)
	if err != nil || result == nil || len(result.Entries) != 1 || !reflect.DeepEqual(result.Entries[0], original) || result.NextPosition == nil || result.NextPosition.ID != original.Entry.ID || !result.NextPosition.LastOpenedAt.Equal(original.LastOpenedAt) {
		t.Fatal("祖先读取期间的源变更影响最近列表或访问位置", err)
	}
}

func TestRecentEntryListServicePositionBounds(t *testing.T) {
	for _, mode := range []string{"smaller ID at same time", "older access with larger IDs", "repeated bound", "larger ID than bound", "newer access than bound"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 让读取依赖返回固定候选，直接检查服务对访问位置的约束。
			actor, reader, query := recentEntryListFixture(2, nil)
			reader.pages = []*domain.RecentEntryPage{{Entries: reader.candidates}}
			before := domain.RecentEntryPosition{ID: 2001, LastOpenedAt: reader.candidates[0].LastOpenedAt}
			valid := false
			switch mode {
			case "smaller ID at same time":
				valid = true
			case "older access with larger IDs":
				before.ID, before.LastOpenedAt, valid = 1, before.LastOpenedAt.Add(time.Second), true
			case "repeated bound":
				before.ID = 2000
			case "larger ID than bound":
				before.ID = 1900
			case "newer access than bound":
				before.LastOpenedAt = before.LastOpenedAt.Add(-time.Second)
			}
			listing := NewRecentEntryListService(func(ctx context.Context, action func(context.Context, RecentEntryListReader) error) error {
				return action(ctx, reader)
			})
			result, err := listing.List(context.Background(), actor, query, &before)
			if valid {
				if err != nil || result == nil || len(result.Entries) != 2 || result.HasMore || result.NextPosition != nil {
					t.Fatal("合法访问位置被拒绝", err)
				}
			} else if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("访问位置未严格推进", err)
			}
		})
	}
}
