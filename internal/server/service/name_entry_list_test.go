package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type nameEntryListRequest struct {
	ctx    context.Context
	userID uint64
	query  domain.GlobalEntryQuery
	after  *domain.NameEntryPosition
}

type nameEntryListReaderStub struct {
	*visibilityReaderStub
	candidates     []domain.NameEntryCandidate
	requests       []nameEntryListRequest
	pages          []*domain.NameEntryPage
	readError      error
	afterRead      func()
	mutatePosition bool
}

func (reader *nameEntryListReaderStub) ListUserNameEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, after *domain.NameEntryPosition) (*domain.NameEntryPage, error) {
	reader.requests = append(reader.requests, nameEntryListRequest{ctx, userID, query, copyNameEntryPosition(after)})
	var page *domain.NameEntryPage
	if reader.pages != nil {
		index := len(reader.requests) - 1
		if index < len(reader.pages) {
			page = reader.pages[index]
		}
	} else {
		entries := make([]domain.NameEntryCandidate, 0)
		for _, candidate := range reader.candidates {
			if after == nil || bytes.Compare(candidate.NameKey, after.NameKey) > 0 || (bytes.Equal(candidate.NameKey, after.NameKey) && candidate.Entry.ID > after.ID) {
				entries = append(entries, candidate)
			}
		}
		more := len(entries) > query.PageSize
		if more {
			entries = entries[:query.PageSize]
		}
		page = &domain.NameEntryPage{Entries: entries, HasMore: more}
	}
	if reader.afterRead != nil {
		reader.afterRead()
	}
	if reader.mutatePosition && after != nil {
		after.ID = 0
		after.NameKey[0] = 0
		after.NameKey = nil
	}
	return page, reader.readError
}

func nameEntryListFixture(count int, hidden func(int) bool) (domain.Actor, *nameEntryListReaderStub, domain.GlobalEntryQuery) {
	// 1. 复用已有完整父链夹具，给每个候选附加独立名称键。
	actor, baseReader, query := globalEntryListFixture(count, hidden)
	reader := &nameEntryListReaderStub{visibilityReaderStub: baseReader.visibilityReaderStub}
	for index, entry := range baseReader.candidates {
		reader.candidates = append(reader.candidates, domain.NameEntryCandidate{Entry: entry, NameKey: []byte(fmt.Sprintf("key-%04d", index))})
	}
	query.Scope, query.Sort = domain.GlobalEntryScopeAll, domain.GlobalEntrySortNameAsc
	return actor, reader, query
}

func TestNameEntryListServiceResults(t *testing.T) {
	for _, scope := range []string{"all", "starred"} {
		t.Run(scope, func(t *testing.T) { testNameEntryListServiceResults(t, scope) })
	}
}

func testNameEntryListServiceResults(t *testing.T, scope string) {
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
			// 1. 组合真实可见性函数，核对名称键和每批原查询条件。
			actor, reader, query := nameEntryListFixture(fixture.count, fixture.hidden)
			if fixture.pageSize != 0 {
				query.PageSize = fixture.pageSize
			}
			query.Scope = scope
			query.Search, query.Kind = "笔记%_", "document"
			original := query
			ctx := context.WithValue(context.Background(), struct{}{}, "request")
			snapshotContext := context.WithValue(ctx, struct{}{}, "snapshot")
			calls := 0
			listing := NewNameEntryListService(func(received context.Context, action func(context.Context, NameEntryListReader) error) error {
				calls++
				if received != ctx {
					t.Error("名称列表快照上下文错误")
				}
				return action(snapshotContext, reader)
			})
			result, err := listing.List(ctx, actor, query, nil)
			if err != nil || result == nil || result.Entries == nil || len(result.Entries) != len(fixture.ids) || result.HasMore != fixture.more || calls != 1 || len(reader.requests) != fixture.readCount || query != original {
				t.Fatal("名称列表、补读或快照错误", err)
			}
			for index, entryID := range fixture.ids {
				if result.Entries[index].Entry.ID != entryID || !reflect.DeepEqual(result.Entries[index], reader.candidates[2000-entryID]) {
					t.Fatal("可见文件顺序或名称键与原候选不符")
				}
			}
			if (result.NextPosition != nil) != fixture.more || (fixture.more && (result.NextPosition.ID != fixture.nextID || !result.NextPosition.IsValid() || !bytes.Equal(result.NextPosition.NameKey, reader.candidates[2000-fixture.nextID].NameKey))) {
				t.Fatal("名称列表最后消费位置错误")
			}
			for _, request := range reader.requests {
				expected := query
				expected.PageSize = 100
				if request.ctx != snapshotContext || request.userID != actor.UserID || request.query != expected {
					t.Fatal("名称候选批次丢失身份、上下文或筛选条件")
				}
			}
		})
	}
}

func TestNameEntryListServiceContinuationAndCopies(t *testing.T) {
	// 1. 页满后续读未消费尾部，读取依赖修改位置时保护原始键。
	actor, reader, query := nameEntryListFixture(6, func(index int) bool { return index%2 == 0 })
	listing := NewNameEntryListService(func(ctx context.Context, action func(context.Context, NameEntryListReader) error) error {
		return action(ctx, reader)
	})
	first, err := listing.List(context.Background(), actor, query, nil)
	if err != nil || first == nil || first.NextPosition == nil {
		t.Fatal(err)
	}
	position := copyNameEntryPosition(first.NextPosition)
	original := copyNameEntryPosition(position)
	reader.mutatePosition = true
	second, err := listing.List(context.Background(), actor, query, position)
	if err != nil || second == nil || len(second.Entries) != 1 || second.Entries[0].Entry.ID != 1995 || second.HasMore || second.NextPosition != nil || !reflect.DeepEqual(position, original) || !reflect.DeepEqual(first.NextPosition, original) {
		t.Fatal("名称续页丢失候选或修改了来源位置", err)
	}
	// 2. 返回名称键、父目录和续页键各自独立持有。
	*first.Entries[0].Entry.ParentID = 999
	first.Entries[0].Entry.Name = "本地副本"
	first.Entries[0].NameKey[0] = 'x'
	if !bytes.Equal(first.NextPosition.NameKey, original.NameKey) {
		t.Fatal("结果键与续页键共享字节")
	}
	first.NextPosition.NameKey[0] = 'z'
	if *reader.candidates[1].Entry.ParentID != 12 || reader.candidates[1].Entry.Name != "学习笔记.md" || string(reader.candidates[1].NameKey) != "key-0001" || !reflect.DeepEqual(position, original) {
		t.Fatal("名称结果或续页键共享来源")
	}
}

func TestNameEntryListServiceFailures(t *testing.T) {
	readFailure, finishFailure := errors.New("fixture read failure"), errors.New("fixture snapshot failure")
	for _, mode := range []string{"nil page", "short has more", "too many", "wrong owner", "inactive", "directory size", "invalid type", "zero id", "zero revision", "nil parent", "zero parent", "empty name", "invalid modified time", "empty key", "oversized key", "descending keys", "equal key descending IDs", "duplicate ID later key", "cross batch duplicate", "read failure", "later read failure", "visibility failure", "disabled empty", "snapshot failure", "no callback", "nil snapshot context", "nil reader", "cancelled snapshot", "cancel after read", "cancel after failed read", "cancel during ancestors", "cancel after callback"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader, query := nameEntryListFixture(2, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(domain.ErrInvalidFileEntryData)
			switch mode {
			case "nil page":
				reader.pages = []*domain.NameEntryPage{nil}
			case "short has more":
				reader.pages = []*domain.NameEntryPage{{Entries: reader.candidates[:1], HasMore: true}}
			case "too many":
				_, many, _ := nameEntryListFixture(101, nil)
				reader.pages = []*domain.NameEntryPage{{Entries: many.candidates}}
			case "wrong owner":
				reader.candidates[0].Entry.UserID = 8
			case "inactive":
				reader.candidates[0].Entry.Status = "TRASHED"
			case "directory size":
				reader.candidates[0].Entry.EntryType = "DIR"
			case "invalid type":
				reader.candidates[0].Entry.EntryType = "LINK"
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
			case "empty key":
				reader.candidates[0].NameKey = nil
			case "oversized key":
				reader.candidates[0].NameKey = bytes.Repeat([]byte{1}, 769)
			case "descending keys":
				reader.candidates[0], reader.candidates[1] = reader.candidates[1], reader.candidates[0]
			case "equal key descending IDs":
				reader.candidates[1].NameKey = bytes.Clone(reader.candidates[0].NameKey)
			case "duplicate ID later key":
				reader.candidates[1].Entry.ID = reader.candidates[0].Entry.ID
				reader.candidates[1].NameKey = []byte("z")
			case "cross batch duplicate":
				_, many, _ := nameEntryListFixture(100, func(int) bool { return true })
				repeated := many.candidates[0]
				repeated.NameKey = []byte("z")
				reader.pages = []*domain.NameEntryPage{{Entries: many.candidates, HasMore: true}, {Entries: []domain.NameEntryCandidate{repeated}}}
			case "read failure":
				reader.readError, want = readFailure, readFailure
			case "later read failure":
				_, many, _ := nameEntryListFixture(101, func(index int) bool { return index != 0 })
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
			listing := NewNameEntryListService(func(current context.Context, action func(context.Context, NameEntryListReader) error) error {
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
				t.Fatal("名称列表异常或取消没有整体失败", err)
			}
		})
	}
}

func TestNameEntryListServiceInputs(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "invalid query", "directory scope", "recent scope", "modified sort", "size sort", "invalid UTF-8", "invalid kind", "zero position ID", "empty position key", "oversized position key", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			actor, _, query := nameEntryListFixture(0, nil)
			var ctx context.Context = context.Background()
			var after *domain.NameEntryPosition
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
			case "directory scope":
				query.Scope = "directory"
			case "recent scope":
				query.Scope, query.Sort = "recent", "last_opened_desc"
			case "modified sort":
				query.Sort = "modified_desc"
			case "size sort":
				query.Sort = "size_desc"
			case "invalid UTF-8":
				query.Search = string([]byte{0xff})
			case "invalid kind":
				query.Kind = "private-kind"
			case "zero position ID":
				after = &domain.NameEntryPosition{NameKey: []byte("key")}
			case "empty position key":
				after = &domain.NameEntryPosition{ID: 19}
			case "oversized position key":
				after = &domain.NameEntryPosition{NameKey: bytes.Repeat([]byte{1}, 769), ID: 19}
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx, want = cancelled, context.Canceled
			case "expired":
				expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				ctx, want = expired, context.DeadlineExceeded
			}
			calls := 0
			listing := NewNameEntryListService(func(context.Context, func(context.Context, NameEntryListReader) error) error { calls++; return nil })
			result, err := listing.List(ctx, actor, query, after)
			if result != nil || err == nil || calls != 0 || (want != nil && !errors.Is(err, want)) {
				t.Fatal("名称非法输入进入快照或取消原因错误", err)
			}
		})
	}
}

func TestNameEntryListServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少名称列表快照依赖未拒绝")
		}
	}()
	NewNameEntryListService(nil)
}

func TestNameEntryListServiceSnapshotCopies(t *testing.T) {
	// 1. 在祖先读取期间修改源候选，验证文件和名称键已独立复制。
	actor, reader, query := nameEntryListFixture(2, nil)
	query.PageSize = 1
	original := reader.candidates[0]
	original.Entry.ParentID = copyOptionalValue(original.Entry.ParentID)
	original.NameKey = bytes.Clone(original.NameKey)
	reader.afterBatch = func() {
		*reader.candidates[0].Entry.ParentID = 999
		reader.candidates[0].Entry.Name = "后续源修改"
		reader.candidates[0].NameKey[0] = 'z'
	}
	listing := NewNameEntryListService(func(ctx context.Context, action func(context.Context, NameEntryListReader) error) error {
		return action(ctx, reader)
	})
	result, err := listing.List(context.Background(), actor, query, nil)
	if err != nil || result == nil || len(result.Entries) != 1 || !reflect.DeepEqual(result.Entries[0], original) || result.NextPosition == nil || result.NextPosition.ID != original.Entry.ID || !bytes.Equal(result.NextPosition.NameKey, original.NameKey) {
		t.Fatal("祖先读取期间的源变更影响名称列表或访问位置", err)
	}
}

func TestNameEntryListServicePositionBounds(t *testing.T) {
	for _, mode := range []string{"same key greater IDs", "greater key smaller IDs", "repeated bound", "smaller ID than bound", "smaller key than bound", "maximum binary key"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader, query := nameEntryListFixture(2, nil)
			reader.candidates[0].NameKey = []byte("key")
			reader.candidates[1].NameKey = []byte("key")
			reader.candidates[1].Entry.ID = 2001
			reader.pages = []*domain.NameEntryPage{{Entries: reader.candidates}}
			after := domain.NameEntryPosition{NameKey: []byte("key"), ID: 1999}
			valid := false
			switch mode {
			case "same key greater IDs":
				valid = true
			case "greater key smaller IDs":
				after.NameKey, after.ID, valid = []byte("a"), ^uint64(0), true
			case "repeated bound":
				after.ID = 2000
			case "smaller ID than bound":
				after.ID = 2002
			case "smaller key than bound":
				after.NameKey = []byte("z")
			case "maximum binary key":
				after.NameKey = []byte{0xff}
				reader.candidates[0].NameKey = bytes.Repeat([]byte{0xff}, 768)
				reader.candidates[1].NameKey = bytes.Repeat([]byte{0xff}, 768)
				valid = true
			}
			original := copyNameEntryPosition(&after)
			listing := NewNameEntryListService(func(ctx context.Context, action func(context.Context, NameEntryListReader) error) error {
				return action(ctx, reader)
			})
			result, err := listing.List(context.Background(), actor, query, &after)
			if valid {
				if err != nil || result == nil || len(result.Entries) != 2 || result.HasMore || result.NextPosition != nil {
					t.Fatal("合法名称位置被拒绝", err)
				}
			} else if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("名称位置未严格推进", err)
			}
			if !reflect.DeepEqual(&after, original) {
				t.Fatal("位置校验改变输入键")
			}
		})
	}
}

func TestNameEntryListServiceDirectories(t *testing.T) {
	for _, scope := range []string{"all", "starred"} {
		t.Run(scope, func(t *testing.T) {
			// 1. 普通目录也参与名称排序，保留筛选和名称键。
			actor, reader, query := nameEntryListFixture(2, nil)
			query.Scope, query.Kind = scope, "folder"
			for index := range reader.candidates {
				reader.candidates[index].Entry.EntryType = "DIR"
				reader.candidates[index].Entry.FileSize = 0
			}
			listing := NewNameEntryListService(func(ctx context.Context, action func(context.Context, NameEntryListReader) error) error {
				return action(ctx, reader)
			})
			result, err := listing.List(context.Background(), actor, query, nil)
			if err != nil || result == nil || len(result.Entries) != 2 || result.HasMore || result.NextPosition != nil || reader.requests[0].query.Kind != "folder" {
				t.Fatal("名称目录列表交付错误", err)
			}
			for index, candidate := range result.Entries {
				if candidate.Entry.EntryType != "DIR" || !reflect.DeepEqual(candidate, reader.candidates[index]) {
					t.Fatal("名称目录数据或名称键丢失")
				}
			}
		})
	}
}
