package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func sizeEntryPageFixture() (domain.Actor, domain.GlobalEntryQuery, *domain.SizeEntryPosition, *domain.SizeEntryList) {
	actor, reader := fileEntryPathFixture()
	first := *reader.entries[13]
	first.ID, first.FileSize = 19, 2048
	second := first
	second.ID, second.ParentID = 18, copyOptionalValue(first.ParentID)
	before := &domain.SizeEntryPosition{FileSize: first.FileSize, ID: 20}
	listed := &domain.SizeEntryList{
		Entries: []domain.FileEntry{first, second}, HasMore: true,
		NextPosition: &domain.SizeEntryPosition{FileSize: first.FileSize, ID: 17},
	}
	query := domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeAll, Sort: domain.GlobalEntrySortSizeDesc, Search: "学习%_", Kind: "document", PageSize: 2}
	return actor, query, before, listed
}

func TestSizeEntryPageServiceFlow(t *testing.T) {
	testSizeEntryPageServiceFlow(t, domain.GlobalEntryScopeAll)
}

func TestStarredSizeEntryPageServiceFlow(t *testing.T) {
	testSizeEntryPageServiceFlow(t, domain.GlobalEntryScopeStarred)
}

func testSizeEntryPageServiceFlow(t *testing.T, scope string) {
	t.Helper()
	for _, mode := range []string{"first page", "next page", "empty end", "full end", "short continuation", "empty first continuation", "empty next continuation", "empty next end", "zero sizes", "smaller next size larger id", "maximum ids", "maximum cursor length", "maximum input length", "next equals visible", "size order overrides ids", "maximum size", "directory result", "folder query", "empty filters", "maximum page size"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 按场景组合三个能力，检查参数、顺序和空页续页。
			actor, query, position, listed := sizeEntryPageFixture()
			query.Scope = scope
			cursor := ""
			wantVerify, wantSign := 0, 1
			signedText := "fixture-next"
			switch mode {
			case "next page", "empty next continuation", "empty next end", "zero sizes", "maximum ids", "maximum input length", "maximum size":
				cursor, wantVerify = "fixture-current", 1
			}
			switch mode {
			case "empty end", "empty next end":
				listed.Entries, listed.HasMore, listed.NextPosition, wantSign = nil, false, nil, 0
			case "full end":
				listed.HasMore, listed.NextPosition, wantSign = false, nil, 0
			case "short continuation":
				listed.Entries = listed.Entries[:1]
			case "empty first continuation", "empty next continuation":
				listed.Entries = nil
			case "zero sizes":
				position.FileSize, listed.NextPosition.FileSize = 0, 0
				listed.Entries[0].FileSize, listed.Entries[1].FileSize = 0, 0
			case "smaller next size larger id":
				listed.NextPosition.ID, listed.NextPosition.FileSize = 999, 1024
			case "maximum ids":
				maximum := ^uint64(0)
				actor.UserID, position.ID = maximum, maximum
				listed.Entries[0].ID, listed.Entries[1].ID = maximum-1, maximum-2
				listed.Entries[0].UserID, listed.Entries[1].UserID = maximum, maximum
				listed.NextPosition.ID = maximum - 3
			case "maximum cursor length":
				signedText = strings.Repeat("a", domain.MaxSizeEntryCursorLength)
			case "maximum input length":
				cursor = strings.Repeat("a", domain.MaxSizeEntryCursorLength)
			case "next equals visible":
				listed.NextPosition.ID = listed.Entries[1].ID
			case "size order overrides ids":
				listed.Entries[0].FileSize, listed.Entries[1].FileSize = 2048, 1024
				listed.Entries[1].ID, listed.NextPosition.FileSize = 999, 512
			case "maximum size":
				position.FileSize = ^uint64(0)
				listed.Entries[0].FileSize, listed.Entries[1].FileSize = ^uint64(0), 1<<63
				listed.NextPosition.FileSize = 0
			case "directory result", "folder query":
				query.Kind = ""
				if mode == "folder query" {
					query.Kind = "folder"
				}
				for index := range listed.Entries {
					listed.Entries[index].EntryType, listed.Entries[index].FileSize = "DIR", 0
				}
				listed.NextPosition.FileSize = 0
			case "empty filters":
				query.Search, query.Kind = "", ""
			case "maximum page size":
				query.PageSize = 100
				first := listed.Entries[0]
				listed.Entries = make([]domain.FileEntry, 100)
				for index := range listed.Entries {
					entry := first
					entry.ID = uint64(200 - index)
					listed.Entries[index] = entry
				}
				listed.NextPosition.ID = 100
			}
			originalPosition := *position
			originalNext := copyOptionalValue(listed.NextPosition)
			expected := copyFileEntryRecords(listed.Entries)
			ctx := context.WithValue(context.Background(), struct{}{}, "request")
			var calls []string
			paging := NewSizeEntryPageService(
				func(userID uint64, received domain.GlobalEntryQuery, token string) (*domain.SizeEntryPosition, error) {
					calls = append(calls, "verify")
					if userID != actor.UserID || received != query || token != cursor {
						t.Error("校验参数错误")
					}
					return position, nil
				},
				func(current context.Context, received domain.Actor, filters domain.GlobalEntryQuery, before *domain.SizeEntryPosition) (*domain.SizeEntryList, error) {
					calls = append(calls, "list")
					if current != ctx || received != actor || filters != query || (before != nil) != (wantVerify == 1) {
						t.Error("列表参数错误")
					}
					if before != nil {
						if before == position || before.ID != position.ID || before.FileSize != position.FileSize {
							t.Error("大小起始位置副本错误")
						}
						before.ID = 0
					}
					return listed, nil
				},
				func(userID uint64, filters domain.GlobalEntryQuery, next domain.SizeEntryPosition) (string, error) {
					calls = append(calls, "sign")
					if userID != actor.UserID || filters != query || next.ID != originalNext.ID || next.FileSize != originalNext.FileSize {
						t.Error("下一位置或绑定错误")
					}
					if len(listed.Entries) > 0 {
						listed.Entries[0].Name, listed.Entries[0].FileSize = "后续源修改", 0
						*listed.Entries[0].ParentID = 999
					}
					listed.NextPosition.ID = 0
					return signedText, nil
				},
			)
			result, err := paging.List(ctx, actor, query, cursor)
			if err != nil || result == nil || result.Entries == nil || !reflect.DeepEqual(result.Entries, expected) || position.ID != originalPosition.ID || position.FileSize != originalPosition.FileSize {
				t.Fatal("分页结果、字段副本或输入保护错误", err)
			}
			wantedCalls := []string{"list"}
			if wantVerify == 1 {
				wantedCalls = append([]string{"verify"}, wantedCalls...)
			}
			if wantSign == 1 {
				wantedCalls = append(wantedCalls, "sign")
			}
			if !reflect.DeepEqual(calls, wantedCalls) || (result.NextCursor != nil) != (wantSign == 1) || (wantSign == 1 && *result.NextCursor != signedText) {
				t.Fatal("调用顺序或续页文本错误", calls)
			}
		})
	}
}

func TestSizeEntryPageServiceFailures(t *testing.T) {
	testSizeEntryPageServiceFailures(t, domain.GlobalEntryScopeAll)
}

func TestStarredSizeEntryPageServiceFailures(t *testing.T) {
	testSizeEntryPageServiceFailures(t, domain.GlobalEntryScopeStarred)
}

func testSizeEntryPageServiceFailures(t *testing.T, scope string) {
	t.Helper()
	fixtureFailure := errors.New("fixture failure")
	for _, mode := range []string{
		"verify error", "nil verify", "zero verify id", "list error", "nil list", "large list", "missing next", "unexpected next",
		"wrong owner", "inactive", "root result", "zero parent", "self parent", "zero id", "zero revision", "empty name", "invalid name", "bad type", "directory size", "bad entry time",
		"ascending", "ascending size", "duplicate smaller size", "entry before bound", "entry size before bound", "zero next", "next before visible", "next size before visible", "empty next no progress", "empty next larger size", "modified precision",
		"sign error", "empty signature", "large signature", "large UTF-8 signature", "cancel verify", "cancel failed verify", "cancel list", "cancel failed list", "cancel sign", "cancel failed sign",
	} {
		t.Run(mode, func(t *testing.T) {
			actor, query, position, listed := sizeEntryPageFixture()
			query.Scope = scope
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(domain.ErrInvalidFileEntryData)
			verifyCalls, listCalls, signCalls := 0, 0, 0
			cursor := "fixture-current"
			switch mode {
			case "verify error":
				want = domain.ErrInvalidSizeEntryCursor
			case "list error", "sign error":
				want = fixtureFailure
			case "nil list":
				listed = nil
			case "large list":
				listed.Entries = append(listed.Entries, listed.Entries[0])
			case "missing next":
				listed.NextPosition = nil
			case "unexpected next":
				listed.HasMore = false
			case "wrong owner":
				listed.Entries[0].UserID = 8
			case "inactive":
				listed.Entries[0].Status = "TRASHED"
			case "root result":
				listed.Entries[0].ParentID = nil
			case "zero parent":
				listed.Entries[0].ParentID = new(uint64)
			case "self parent":
				listed.Entries[0].ParentID = &listed.Entries[0].ID
			case "zero id":
				listed.Entries[0].ID = 0
			case "zero revision":
				listed.Entries[0].Revision = 0
			case "empty name":
				listed.Entries[0].Name = ""
			case "invalid name":
				listed.Entries[0].Name = string([]byte{0xff})
			case "bad type":
				listed.Entries[0].EntryType = "LINK"
			case "directory size":
				listed.Entries[0].EntryType = "DIR"
			case "bad entry time":
				listed.Entries[0].ModifiedAt = time.Time{}
			case "ascending":
				listed.Entries[0], listed.Entries[1] = listed.Entries[1], listed.Entries[0]
			case "ascending size":
				listed.Entries[1].FileSize++
			case "duplicate smaller size":
				listed.Entries[1].ID = listed.Entries[0].ID
				listed.Entries[1].FileSize = listed.Entries[0].FileSize - 1
			case "entry before bound":
				listed.Entries[0].ID = position.ID
			case "entry size before bound":
				listed.Entries[0].FileSize = position.FileSize + 1
			case "zero next":
				listed.NextPosition.ID = 0
			case "next before visible":
				listed.NextPosition.ID = 19
			case "next size before visible":
				listed.NextPosition.FileSize++
			case "empty next no progress":
				listed.Entries = nil
				listed.NextPosition.ID = position.ID
			case "empty next larger size":
				listed.Entries = nil
				listed.NextPosition.FileSize = position.FileSize + 1
			case "modified precision":
				listed.Entries[0].ModifiedAt = listed.Entries[0].ModifiedAt.Add(time.Nanosecond)
			case "empty signature", "large signature", "large UTF-8 signature":
				want = nil
			case "cancel verify", "cancel failed verify", "cancel list", "cancel failed list", "cancel sign", "cancel failed sign":
				want = context.Canceled
			}
			paging := NewSizeEntryPageService(
				func(uint64, domain.GlobalEntryQuery, string) (*domain.SizeEntryPosition, error) {
					verifyCalls++
					switch mode {
					case "verify error":
						return nil, domain.ErrInvalidSizeEntryCursor
					case "nil verify":
						return nil, nil
					case "zero verify id":
						return &domain.SizeEntryPosition{FileSize: position.FileSize}, nil
					case "cancel verify":
						cancel()
					case "cancel failed verify":
						cancel()
						return nil, fixtureFailure
					}
					return position, nil
				},
				func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.SizeEntryPosition) (*domain.SizeEntryList, error) {
					listCalls++
					if mode == "cancel list" || mode == "cancel failed list" {
						cancel()
					}
					if mode == "list error" || mode == "cancel failed list" {
						return nil, fixtureFailure
					}
					return listed, nil
				},
				func(uint64, domain.GlobalEntryQuery, domain.SizeEntryPosition) (string, error) {
					signCalls++
					if mode == "cancel sign" || mode == "cancel failed sign" {
						cancel()
					}
					if mode == "sign error" || mode == "cancel failed sign" {
						return "", fixtureFailure
					}
					if mode == "empty signature" {
						return "", nil
					}
					if mode == "large signature" {
						return strings.Repeat("a", domain.MaxSizeEntryCursorLength+1), nil
					}
					if mode == "large UTF-8 signature" {
						return strings.Repeat("界", domain.MaxSizeEntryCursorLength/3+1), nil
					}
					return "fixture-next", nil
				},
			)
			result, err := paging.List(ctx, actor, query, cursor)
			if result != nil || err == nil || (want != nil && !errors.Is(err, want)) || verifyCalls != 1 {
				t.Fatal("错误或取消未整体失败", err)
			}
			if (strings.Contains(mode, "verify") && listCalls != 0) || (listCalls == 0 && signCalls != 0) {
				t.Fatal("前置失败仍调用下游")
			}
		})
	}
}

func TestSizeEntryPageServiceInputs(t *testing.T) {
	testSizeEntryPageServiceInputs(t, domain.GlobalEntryScopeAll)
}

func TestStarredSizeEntryPageServiceInputs(t *testing.T) {
	testSizeEntryPageServiceInputs(t, domain.GlobalEntryScopeStarred)
}

func testSizeEntryPageServiceInputs(t *testing.T, scope string) {
	t.Helper()
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero page", "large page", "recent", "starred name sort", "name sort", "invalid search", "invalid kind", "directory scope", "large cursor", "large UTF-8 cursor", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			actor, query, _, _ := sizeEntryPageFixture()
			query.Scope = scope
			var ctx context.Context = context.Background()
			cursor := ""
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
			case "zero page":
				query.PageSize = 0
			case "large page":
				query.PageSize = 101
			case "recent":
				query.Scope, query.Sort = domain.GlobalEntryScopeRecent, domain.GlobalEntrySortLastOpenedDesc
			case "starred name sort":
				query.Scope, query.Sort = domain.GlobalEntryScopeStarred, domain.GlobalEntrySortNameAsc
			case "name sort":
				query.Sort = domain.GlobalEntrySortNameAsc
			case "invalid search":
				query.Search = string([]byte{0xff})
			case "invalid kind":
				query.Kind = "private-kind"
			case "directory scope":
				query.Scope = "directory"
			case "large UTF-8 cursor":
				cursor, want = strings.Repeat("界", domain.MaxSizeEntryCursorLength/3+1), domain.ErrInvalidSizeEntryCursor
			case "large cursor":
				cursor, want = strings.Repeat("a", domain.MaxSizeEntryCursorLength+1), domain.ErrInvalidSizeEntryCursor
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
			paging := NewSizeEntryPageService(
				func(uint64, domain.GlobalEntryQuery, string) (*domain.SizeEntryPosition, error) {
					calls++
					return nil, nil
				},
				func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.SizeEntryPosition) (*domain.SizeEntryList, error) {
					calls++
					return nil, nil
				},
				func(uint64, domain.GlobalEntryQuery, domain.SizeEntryPosition) (string, error) {
					calls++
					return "", nil
				},
			)
			result, err := paging.List(ctx, actor, query, cursor)
			if result != nil || err == nil || calls != 0 || (want != nil && !errors.Is(err, want)) {
				t.Fatal("非法输入调用了依赖或错误原因不符", err)
			}
		})
	}
}

func TestSizeEntryPageServiceDependencies(t *testing.T) {
	for _, missing := range []string{"verify", "list", "sign"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖未拒绝")
				}
			}()
			verify := func(uint64, domain.GlobalEntryQuery, string) (*domain.SizeEntryPosition, error) { return nil, nil }
			list := func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.SizeEntryPosition) (*domain.SizeEntryList, error) {
				return nil, nil
			}
			sign := func(uint64, domain.GlobalEntryQuery, domain.SizeEntryPosition) (string, error) { return "", nil }
			switch missing {
			case "verify":
				verify = nil
			case "list":
				list = nil
			case "sign":
				sign = nil
			}
			NewSizeEntryPageService(verify, list, sign)
		})
	}
}
