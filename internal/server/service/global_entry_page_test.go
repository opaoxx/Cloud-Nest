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

func globalEntryPageFixture() (domain.Actor, domain.GlobalEntryQuery, *domain.FileEntryPosition, *domain.GlobalEntryList) {
	actor, reader := fileEntryPathFixture()
	first := *reader.entries[13]
	first.ID = 19
	second := first
	second.ID, second.ParentID = 18, copyOptionalValue(first.ParentID)
	before := &domain.FileEntryPosition{ModifiedAt: first.ModifiedAt, ID: 20}
	listed := &domain.GlobalEntryList{
		Entries: []domain.FileEntry{first, second}, HasMore: true,
		NextPosition: &domain.FileEntryPosition{ModifiedAt: first.ModifiedAt, ID: 17},
	}
	query := domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeAll, Sort: domain.GlobalEntrySortModifiedDesc, Search: "学习%_", Kind: "document", PageSize: 2}
	return actor, query, before, listed
}

func TestGlobalEntryPageServiceFlow(t *testing.T) {
	for _, mode := range []string{"first page", "next page", "empty end", "full end", "short continuation", "empty first continuation", "empty next continuation", "empty next end", "offset positions", "older next larger id", "maximum ids", "maximum cursor length"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 按场景组合三个能力，检查参数、顺序和空页续页。
			actor, query, position, listed := globalEntryPageFixture()
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
			case "full end":
				listed.HasMore, listed.NextPosition, wantSign = false, nil, 0
			case "short continuation":
				listed.Entries = listed.Entries[:1]
			case "empty first continuation", "empty next continuation":
				listed.Entries = nil
			case "offset positions":
				position.ModifiedAt = position.ModifiedAt.In(time.FixedZone("fixture", 8*3600))
				listed.NextPosition.ModifiedAt = listed.NextPosition.ModifiedAt.In(time.FixedZone("fixture", 8*3600))
			case "older next larger id":
				listed.NextPosition.ID = 999
				listed.NextPosition.ModifiedAt = listed.NextPosition.ModifiedAt.Add(-time.Second)
			case "maximum ids":
				maximum := ^uint64(0)
				actor.UserID, position.ID = maximum, maximum
				listed.Entries[0].ID, listed.Entries[1].ID = maximum-1, maximum-2
				listed.Entries[0].UserID, listed.Entries[1].UserID = maximum, maximum
				listed.NextPosition.ID = maximum - 3
			case "maximum cursor length":
				signedText = strings.Repeat("a", domain.MaxGlobalEntryCursorLength)
			}
			originalPosition := *position
			originalNext := copyOptionalValue(listed.NextPosition)
			expected := copyFileEntryRecords(listed.Entries)
			ctx := context.WithValue(context.Background(), struct{}{}, "request")
			var calls []string
			paging := NewGlobalEntryPageService(
				func(userID uint64, received domain.GlobalEntryQuery, token string) (*domain.FileEntryPosition, error) {
					calls = append(calls, "verify")
					if userID != actor.UserID || received != query || token != cursor {
						t.Error("校验参数错误")
					}
					return position, nil
				},
				func(current context.Context, received domain.Actor, filters domain.GlobalEntryQuery, before *domain.FileEntryPosition) (*domain.GlobalEntryList, error) {
					calls = append(calls, "list")
					if current != ctx || received != actor || filters != query || (before != nil) != (wantVerify == 1) {
						t.Error("列表参数错误")
					}
					if before != nil {
						if before == position || before.ID != position.ID || !before.ModifiedAt.Equal(position.ModifiedAt) || before.ModifiedAt.Location() != time.UTC {
							t.Error("起始位置副本或 UTC 错误")
						}
						before.ID = 0
					}
					return listed, nil
				},
				func(userID uint64, filters domain.GlobalEntryQuery, next domain.FileEntryPosition) (string, error) {
					calls = append(calls, "sign")
					if userID != actor.UserID || filters != query || next.ID != originalNext.ID || !next.ModifiedAt.Equal(originalNext.ModifiedAt) || next.ModifiedAt.Location() != time.UTC {
						t.Error("下一位置或绑定错误")
					}
					if len(listed.Entries) > 0 {
						listed.Entries[0].Name = "后续源修改"
						*listed.Entries[0].ParentID = 999
					}
					listed.NextPosition.ID = 0
					return signedText, nil
				},
			)
			result, err := paging.List(ctx, actor, query, cursor)
			if err != nil || result == nil || result.Entries == nil || !reflect.DeepEqual(result.Entries, expected) || position.ID != originalPosition.ID || position.ModifiedAt != originalPosition.ModifiedAt {
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

func TestGlobalEntryPageServiceFailures(t *testing.T) {
	fixtureFailure := errors.New("fixture failure")
	for _, mode := range []string{
		"verify error", "nil verify", "zero verify id", "bad verify time", "list error", "nil list", "large list", "missing next", "unexpected next",
		"wrong owner", "inactive", "root result", "zero parent", "zero id", "zero revision", "empty name", "bad type", "directory size", "bad entry time",
		"ascending", "duplicate older id", "entry before bound", "zero next", "bad next time", "next before visible", "empty next no progress",
		"sign error", "empty signature", "large signature", "cancel verify", "cancel failed verify", "cancel list", "cancel failed list", "cancel sign", "cancel failed sign",
	} {
		t.Run(mode, func(t *testing.T) {
			actor, query, position, listed := globalEntryPageFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(domain.ErrInvalidFileEntryData)
			verifyCalls, listCalls, signCalls := 0, 0, 0
			cursor := "fixture-current"
			switch mode {
			case "verify error":
				want = domain.ErrInvalidGlobalEntryCursor
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
			case "zero id":
				listed.Entries[0].ID = 0
			case "zero revision":
				listed.Entries[0].Revision = 0
			case "empty name":
				listed.Entries[0].Name = ""
			case "bad type":
				listed.Entries[0].EntryType = "LINK"
			case "directory size":
				listed.Entries[0].EntryType = "DIR"
			case "bad entry time":
				listed.Entries[0].ModifiedAt = time.Time{}
			case "ascending":
				listed.Entries[0], listed.Entries[1] = listed.Entries[1], listed.Entries[0]
			case "duplicate older id":
				listed.Entries[1].ID = listed.Entries[0].ID
				listed.Entries[1].ModifiedAt = listed.Entries[0].ModifiedAt.Add(-time.Second)
			case "entry before bound":
				listed.Entries[0].ID = position.ID
			case "zero next":
				listed.NextPosition.ID = 0
			case "bad next time":
				listed.NextPosition.ModifiedAt = time.Time{}
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
			paging := NewGlobalEntryPageService(
				func(uint64, domain.GlobalEntryQuery, string) (*domain.FileEntryPosition, error) {
					verifyCalls++
					switch mode {
					case "verify error":
						return nil, domain.ErrInvalidGlobalEntryCursor
					case "nil verify":
						return nil, nil
					case "zero verify id":
						return &domain.FileEntryPosition{ModifiedAt: position.ModifiedAt}, nil
					case "bad verify time":
						return &domain.FileEntryPosition{ID: position.ID}, nil
					case "cancel verify":
						cancel()
					case "cancel failed verify":
						cancel()
						return nil, fixtureFailure
					}
					return position, nil
				},
				func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.FileEntryPosition) (*domain.GlobalEntryList, error) {
					listCalls++
					if mode == "cancel list" || mode == "cancel failed list" {
						cancel()
					}
					if mode == "list error" || mode == "cancel failed list" {
						return nil, fixtureFailure
					}
					return listed, nil
				},
				func(uint64, domain.GlobalEntryQuery, domain.FileEntryPosition) (string, error) {
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
						return strings.Repeat("a", domain.MaxGlobalEntryCursorLength+1), nil
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

func TestGlobalEntryPageServiceInputs(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero page", "large page", "starred", "name sort", "invalid search", "large cursor", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			actor, query, _, _ := globalEntryPageFixture()
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
			case "starred":
				query.Scope = domain.GlobalEntryScopeStarred
			case "name sort":
				query.Sort = domain.GlobalEntrySortNameAsc
			case "invalid search":
				query.Search = string([]byte{0xff})
			case "large cursor":
				cursor, want = strings.Repeat("a", domain.MaxGlobalEntryCursorLength+1), domain.ErrInvalidGlobalEntryCursor
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
			paging := NewGlobalEntryPageService(
				func(uint64, domain.GlobalEntryQuery, string) (*domain.FileEntryPosition, error) {
					calls++
					return nil, nil
				},
				func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.FileEntryPosition) (*domain.GlobalEntryList, error) {
					calls++
					return nil, nil
				},
				func(uint64, domain.GlobalEntryQuery, domain.FileEntryPosition) (string, error) {
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

func TestGlobalEntryPageServiceDependencies(t *testing.T) {
	for _, missing := range []string{"verify", "list", "sign"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖未拒绝")
				}
			}()
			verify := func(uint64, domain.GlobalEntryQuery, string) (*domain.FileEntryPosition, error) { return nil, nil }
			list := func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.FileEntryPosition) (*domain.GlobalEntryList, error) {
				return nil, nil
			}
			sign := func(uint64, domain.GlobalEntryQuery, domain.FileEntryPosition) (string, error) { return "", nil }
			switch missing {
			case "verify":
				verify = nil
			case "list":
				list = nil
			case "sign":
				sign = nil
			}
			NewGlobalEntryPageService(verify, list, sign)
		})
	}
}
