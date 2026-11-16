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

func recentEntryPageFixture() (domain.Actor, domain.GlobalEntryQuery, *domain.RecentEntryPosition, *domain.RecentEntryList) {
	actor, query, before, listed := globalEntryPageFixture()
	query.Scope, query.Sort = domain.GlobalEntryScopeRecent, domain.GlobalEntrySortLastOpenedDesc
	position := &domain.RecentEntryPosition{LastOpenedAt: before.ModifiedAt.Add(time.Hour), ID: before.ID}
	result := &domain.RecentEntryList{
		Entries:      make([]domain.RecentEntryCandidate, 0, len(listed.Entries)),
		HasMore:      listed.HasMore,
		NextPosition: &domain.RecentEntryPosition{LastOpenedAt: listed.NextPosition.ModifiedAt.Add(time.Hour), ID: listed.NextPosition.ID},
	}
	for _, entry := range listed.Entries {
		result.Entries = append(result.Entries, domain.RecentEntryCandidate{Entry: entry, LastOpenedAt: entry.ModifiedAt.Add(time.Hour)})
	}
	return actor, query, position, result
}

func TestRecentEntryPageServiceFlow(t *testing.T) {
	for _, mode := range []string{"first page", "next page", "empty end", "folder empty end", "full end", "short continuation", "empty first continuation", "empty next continuation", "empty next end", "offset positions", "offset entries", "older next larger id", "maximum ids", "maximum cursor length"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 按场景组合三个能力，检查参数、顺序和空页续页。
			actor, query, position, listed := recentEntryPageFixture()
			cursor := ""
			wantVerify, wantSign := 0, 1
			signedText := "fixture-next"
			switch mode {
			case "next page", "empty next continuation", "empty next end", "offset positions", "maximum ids":
				cursor, wantVerify = "fixture-current", 1
			}
			switch mode {
			case "empty end", "empty next end":
				listed.Entries, listed.HasMore, listed.NextPosition, wantSign = nil, false, nil, 0
			case "folder empty end":
				query.Kind = "folder"
				listed.Entries, listed.HasMore, listed.NextPosition, wantSign = nil, false, nil, 0
			case "full end":
				listed.HasMore, listed.NextPosition, wantSign = false, nil, 0
			case "short continuation":
				listed.Entries = listed.Entries[:1]
			case "empty first continuation", "empty next continuation":
				listed.Entries = nil
			case "offset positions":
				position.LastOpenedAt = position.LastOpenedAt.In(time.FixedZone("fixture", 8*3600))
				listed.NextPosition.LastOpenedAt = listed.NextPosition.LastOpenedAt.In(time.FixedZone("fixture", 8*3600))
			case "offset entries":
				for index := range listed.Entries {
					listed.Entries[index].LastOpenedAt = listed.Entries[index].LastOpenedAt.In(time.FixedZone("fixture", 8*3600))
				}
			case "older next larger id":
				listed.NextPosition.ID = 999
				listed.NextPosition.LastOpenedAt = listed.NextPosition.LastOpenedAt.Add(-time.Second)
			case "maximum ids":
				maximum := ^uint64(0)
				actor.UserID, position.ID = maximum, maximum
				listed.Entries[0].Entry.ID, listed.Entries[1].Entry.ID = maximum-1, maximum-2
				listed.Entries[0].Entry.UserID, listed.Entries[1].Entry.UserID = maximum, maximum
				listed.NextPosition.ID = maximum - 3
			case "maximum cursor length":
				signedText = strings.Repeat("a", domain.MaxRecentEntryCursorLength)
			}
			originalPosition := *position
			originalNext := copyOptionalValue(listed.NextPosition)
			expected := append([]domain.RecentEntryCandidate{}, listed.Entries...)
			for index := range expected {
				expected[index].Entry.ParentID = copyOptionalValue(expected[index].Entry.ParentID)
			}
			ctx := context.WithValue(context.Background(), struct{}{}, "request")
			var calls []string
			paging := NewRecentEntryPageService(
				func(userID uint64, received domain.GlobalEntryQuery, token string) (*domain.RecentEntryPosition, error) {
					calls = append(calls, "verify")
					if userID != actor.UserID || received != query || token != cursor {
						t.Error("校验参数错误")
					}
					return position, nil
				},
				func(current context.Context, received domain.Actor, filters domain.GlobalEntryQuery, before *domain.RecentEntryPosition) (*domain.RecentEntryList, error) {
					calls = append(calls, "list")
					if current != ctx || received != actor || filters != query || (before != nil) != (wantVerify == 1) {
						t.Error("列表参数错误")
					}
					if before != nil {
						if before == position || before.ID != position.ID || !before.LastOpenedAt.Equal(position.LastOpenedAt) || before.LastOpenedAt.Location() != time.UTC {
							t.Error("起始位置副本或 UTC 错误")
						}
						before.ID = 0
					}
					return listed, nil
				},
				func(userID uint64, filters domain.GlobalEntryQuery, next domain.RecentEntryPosition) (string, error) {
					calls = append(calls, "sign")
					if userID != actor.UserID || filters != query || next.ID != originalNext.ID || !next.LastOpenedAt.Equal(originalNext.LastOpenedAt) || next.LastOpenedAt.Location() != time.UTC {
						t.Error("下一位置或绑定错误")
					}
					if len(listed.Entries) > 0 {
						listed.Entries[0].Entry.Name = "后续源修改"
						*listed.Entries[0].Entry.ParentID = 999
						listed.Entries[0].LastOpenedAt = time.Time{}
					}
					listed.NextPosition.ID = 0
					return signedText, nil
				},
			)
			result, err := paging.List(ctx, actor, query, cursor)
			if err != nil || result == nil || result.Entries == nil || !reflect.DeepEqual(result.Entries, expected) || position.ID != originalPosition.ID || position.LastOpenedAt != originalPosition.LastOpenedAt {
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

func TestRecentEntryPageServiceFailures(t *testing.T) {
	fixtureFailure := errors.New("fixture failure")
	for _, mode := range []string{
		"verify error", "nil verify", "zero verify id", "bad verify time", "verify precision", "list error", "nil list", "large list", "missing next", "unexpected next",
		"wrong owner", "inactive", "root result", "zero parent", "zero id", "zero revision", "empty name", "bad type", "directory result", "bad access time",
		"ascending", "duplicate older id", "entry before bound", "zero next", "bad next time", "next before visible", "empty next no progress",
		"sign error", "empty signature", "large signature", "cancel verify", "cancel failed verify", "cancel list", "cancel failed list", "cancel sign", "cancel failed sign",
		"bad modified time", "access precision", "folder result", "next precision", "next newer than visible", "access newer than bound",
	} {
		t.Run(mode, func(t *testing.T) {
			actor, query, position, listed := recentEntryPageFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(domain.ErrInvalidFileEntryData)
			verifyCalls, listCalls, signCalls := 0, 0, 0
			cursor := "fixture-current"
			switch mode {
			case "verify error":
				want = domain.ErrInvalidRecentEntryCursor
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
				listed.Entries[0].Entry.UserID = 8
			case "inactive":
				listed.Entries[0].Entry.Status = "TRASHED"
			case "root result":
				listed.Entries[0].Entry.ParentID = nil
			case "zero parent":
				listed.Entries[0].Entry.ParentID = new(uint64)
			case "zero id":
				listed.Entries[0].Entry.ID = 0
			case "zero revision":
				listed.Entries[0].Entry.Revision = 0
			case "empty name":
				listed.Entries[0].Entry.Name = ""
			case "bad type":
				listed.Entries[0].Entry.EntryType = "LINK"
			case "directory result":
				listed.Entries[0].Entry.EntryType = "DIR"
				listed.Entries[0].Entry.FileSize = 0
			case "bad access time":
				listed.Entries[0].LastOpenedAt = time.Time{}
			case "bad modified time":
				listed.Entries[0].Entry.ModifiedAt = time.Time{}
			case "access precision":
				listed.Entries[0].LastOpenedAt = listed.Entries[0].LastOpenedAt.Add(time.Nanosecond)
			case "folder result":
				query.Kind = "folder"
			case "ascending":
				listed.Entries[0], listed.Entries[1] = listed.Entries[1], listed.Entries[0]
			case "duplicate older id":
				listed.Entries[1].Entry.ID = listed.Entries[0].Entry.ID
				listed.Entries[1].LastOpenedAt = listed.Entries[0].LastOpenedAt.Add(-time.Second)
			case "entry before bound":
				listed.Entries[0].Entry.ID = position.ID
			case "zero next":
				listed.NextPosition.ID = 0
			case "bad next time":
				listed.NextPosition.LastOpenedAt = time.Time{}
			case "next precision":
				listed.NextPosition.LastOpenedAt = listed.NextPosition.LastOpenedAt.Add(time.Nanosecond)
			case "next newer than visible":
				listed.NextPosition.LastOpenedAt = listed.Entries[1].LastOpenedAt.Add(time.Microsecond)
			case "access newer than bound":
				listed.Entries[0].LastOpenedAt = position.LastOpenedAt.Add(time.Microsecond)
			case "next before visible":
				listed.NextPosition.ID = 19
			case "empty next no progress":
				listed.Entries = nil
				listed.NextPosition.ID = position.ID
			case "empty signature", "large signature":
				want = nil
			case "cancel verify", "cancel failed verify", "cancel list", "cancel failed list", "cancel sign", "cancel failed sign":
				want = context.Canceled
			}
			paging := NewRecentEntryPageService(
				func(uint64, domain.GlobalEntryQuery, string) (*domain.RecentEntryPosition, error) {
					verifyCalls++
					switch mode {
					case "verify error":
						return nil, domain.ErrInvalidRecentEntryCursor
					case "nil verify":
						return nil, nil
					case "zero verify id":
						return &domain.RecentEntryPosition{LastOpenedAt: position.LastOpenedAt}, nil
					case "bad verify time":
						return &domain.RecentEntryPosition{ID: position.ID}, nil
					case "verify precision":
						return &domain.RecentEntryPosition{ID: position.ID, LastOpenedAt: position.LastOpenedAt.Add(time.Nanosecond)}, nil
					case "cancel verify":
						cancel()
					case "cancel failed verify":
						cancel()
						return nil, fixtureFailure
					}
					return position, nil
				},
				func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.RecentEntryPosition) (*domain.RecentEntryList, error) {
					listCalls++
					if mode == "cancel list" || mode == "cancel failed list" {
						cancel()
					}
					if mode == "list error" || mode == "cancel failed list" {
						return nil, fixtureFailure
					}
					return listed, nil
				},
				func(uint64, domain.GlobalEntryQuery, domain.RecentEntryPosition) (string, error) {
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
						return strings.Repeat("a", domain.MaxRecentEntryCursorLength+1), nil
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

func TestRecentEntryPageServiceInputs(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero page", "large page", "all scope", "starred scope", "starred name sort", "name sort", "invalid search", "invalid kind", "large cursor", "large UTF-8 cursor", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			actor, query, _, _ := recentEntryPageFixture()
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
			case "all scope":
				query.Scope, query.Sort = domain.GlobalEntryScopeAll, domain.GlobalEntrySortModifiedDesc
			case "starred scope":
				query.Scope, query.Sort = domain.GlobalEntryScopeStarred, domain.GlobalEntrySortModifiedDesc
			case "starred name sort":
				query.Scope, query.Sort = domain.GlobalEntryScopeStarred, domain.GlobalEntrySortNameAsc
			case "name sort":
				query.Sort = domain.GlobalEntrySortNameAsc
			case "invalid search":
				query.Search = string([]byte{0xff})
			case "invalid kind":
				query.Kind = "private-kind"
			case "large cursor":
				cursor, want = strings.Repeat("a", domain.MaxRecentEntryCursorLength+1), domain.ErrInvalidRecentEntryCursor
			case "large UTF-8 cursor":
				cursor, want = strings.Repeat("界", domain.MaxRecentEntryCursorLength/3+1), domain.ErrInvalidRecentEntryCursor
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
			paging := NewRecentEntryPageService(
				func(uint64, domain.GlobalEntryQuery, string) (*domain.RecentEntryPosition, error) {
					calls++
					return nil, nil
				},
				func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.RecentEntryPosition) (*domain.RecentEntryList, error) {
					calls++
					return nil, nil
				},
				func(uint64, domain.GlobalEntryQuery, domain.RecentEntryPosition) (string, error) {
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

func TestRecentEntryPageServiceDependencies(t *testing.T) {
	for _, missing := range []string{"verify", "list", "sign"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖未拒绝")
				}
			}()
			verify := func(uint64, domain.GlobalEntryQuery, string) (*domain.RecentEntryPosition, error) { return nil, nil }
			list := func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.RecentEntryPosition) (*domain.RecentEntryList, error) {
				return nil, nil
			}
			sign := func(uint64, domain.GlobalEntryQuery, domain.RecentEntryPosition) (string, error) { return "", nil }
			switch missing {
			case "verify":
				verify = nil
			case "list":
				list = nil
			case "sign":
				sign = nil
			}
			NewRecentEntryPageService(verify, list, sign)
		})
	}
}
