package service

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func nameEntryPageFixture() (domain.Actor, domain.GlobalEntryQuery, *domain.NameEntryPosition, *domain.NameEntryList) {
	actor, query, _, listed := globalEntryPageFixture()
	query.Sort = domain.GlobalEntrySortNameAsc
	listed.Entries[1].ID = 20
	position := &domain.NameEntryPosition{NameKey: []byte("readme.md"), ID: 18}
	result := &domain.NameEntryList{
		Entries: []domain.NameEntryCandidate{
			{Entry: listed.Entries[0], NameKey: []byte("readme.md")},
			{Entry: listed.Entries[1], NameKey: []byte("readme.md")},
		},
		HasMore: true, NextPosition: &domain.NameEntryPosition{NameKey: []byte("readme.md"), ID: 21},
	}
	return actor, query, position, result
}

func TestNameEntryPageServiceFlow(t *testing.T) {
	for _, scope := range []string{"all", "starred"} {
		t.Run(scope, func(t *testing.T) {
			for _, mode := range []string{
				"first page", "next page", "empty end", "full end", "short continuation", "empty first continuation", "empty next continuation", "empty next end",
				"next equals visible", "larger key smaller id", "key order overrides ids", "binary keys", "maximum key", "maximum ids", "maximum cursor length", "maximum input length",
				"directory result", "shared next key", "folder query", "empty filters", "maximum page size", "minimum key",
			} {
				t.Run(mode, func(t *testing.T) {
					// 1. 准备首页、续页及预算页，并保留预期的独立事实。
					actor, query, position, listed := nameEntryPageFixture()
					query.Scope = scope
					cursor, signedText := "", "fixture-next"
					wantVerify, wantSign := false, true
					switch mode {
					case "next page", "empty next continuation", "empty next end", "binary keys", "maximum input length":
						cursor, wantVerify = "fixture-current", true
					}
					switch mode {
					case "empty end", "empty next end":
						listed.Entries, listed.HasMore, listed.NextPosition, wantSign = nil, false, nil, false
					case "full end":
						listed.HasMore, listed.NextPosition, wantSign = false, nil, false
					case "short continuation":
						listed.Entries = listed.Entries[:1]
					case "empty first continuation", "empty next continuation":
						listed.Entries = nil
					case "next equals visible":
						listed.NextPosition.ID = listed.Entries[1].Entry.ID
					case "shared next key":
						listed.NextPosition.ID = listed.Entries[1].Entry.ID
						listed.NextPosition.NameKey = listed.Entries[1].NameKey
					case "larger key smaller id":
						listed.NextPosition.NameKey, listed.NextPosition.ID = []byte("z"), 1
					case "key order overrides ids":
						listed.Entries[0].NameKey, listed.Entries[1].NameKey = []byte("b"), []byte("c")
						listed.Entries[1].Entry.ID, listed.NextPosition.NameKey = 18, []byte("d")
					case "binary keys":
						position.NameKey = []byte{'a', 0}
						listed.Entries[0].NameKey, listed.Entries[1].NameKey = []byte{'a', 0, 0xff}, []byte{0xff}
						listed.NextPosition.NameKey = []byte{0xff, 0}
					case "maximum key":
						listed.NextPosition.NameKey = bytes.Repeat([]byte{0xff}, 768)
					case "maximum ids":
						maximum := ^uint64(0)
						actor.UserID = maximum
						listed.Entries[0].Entry.ID, listed.Entries[1].Entry.ID, listed.NextPosition.ID = maximum-2, maximum-1, maximum
						listed.Entries[0].Entry.UserID, listed.Entries[1].Entry.UserID = maximum, maximum
					case "maximum cursor length":
						signedText = strings.Repeat("a", domain.MaxNameEntryCursorLength)
					case "maximum input length":
						cursor = strings.Repeat("a", domain.MaxNameEntryCursorLength)
					case "directory result", "folder query":
						query.Kind = ""
						if mode == "folder query" {
							query.Kind = "folder"
						}
						for index := range listed.Entries {
							listed.Entries[index].Entry.EntryType, listed.Entries[index].Entry.FileSize = "DIR", 0
						}
					case "empty filters":
						query.Search, query.Kind = "", ""
					case "maximum page size":
						query.PageSize = 100
						first := listed.Entries[0]
						listed.Entries = make([]domain.NameEntryCandidate, 100)
						for index := range listed.Entries {
							candidate := first
							candidate.Entry.ID = uint64(100 + index)
							listed.Entries[index] = candidate
						}
						listed.NextPosition.ID = 200
					case "minimum key":
						listed.Entries[0].NameKey, listed.Entries[1].NameKey = []byte{0}, []byte{0, 0}
						listed.NextPosition.NameKey = []byte{0, 0xff}
					}
					originalPosition, originalNext := copyNameEntryPosition(position), copyNameEntryPosition(listed.NextPosition)
					expected := append([]domain.NameEntryCandidate{}, listed.Entries...)
					for index := range expected {
						expected[index].NameKey = bytes.Clone(expected[index].NameKey)
						expected[index].Entry.ParentID = copyOptionalValue(expected[index].Entry.ParentID)
					}
					ctx := context.WithValue(context.Background(), struct{}{}, "request")
					var calls []string
					paging := NewNameEntryPageService(
						func(userID uint64, received domain.GlobalEntryQuery, token string) (*domain.NameEntryPosition, error) {
							calls = append(calls, "verify")
							if userID != actor.UserID || received != query || token != cursor {
								t.Error("名称校验参数错误")
							}
							return position, nil
						},
						func(current context.Context, received domain.Actor, filters domain.GlobalEntryQuery, after *domain.NameEntryPosition) (*domain.NameEntryList, error) {
							calls = append(calls, "list")
							if current != ctx || received != actor || filters != query || (after != nil) != wantVerify {
								t.Error("名称列表参数错误")
							}
							if after != nil {
								if after == position || !reflect.DeepEqual(after, originalPosition) {
									t.Error("名称起始位置副本错误")
								}
								after.ID, after.NameKey[0] = 0, 0xff
							}
							return listed, nil
						},
						func(userID uint64, filters domain.GlobalEntryQuery, next domain.NameEntryPosition) (string, error) {
							calls = append(calls, "sign")
							if userID != actor.UserID || filters != query || !reflect.DeepEqual(next, *originalNext) {
								t.Error("名称下一页位置或绑定错误")
							}
							// 2. 修改签名输入和上游来源，确认交付结果分别持有键和父目录。
							next.NameKey[0] ^= 1
							if len(listed.Entries) > 0 {
								listed.Entries[0].NameKey[0] ^= 1
								listed.Entries[0].Entry.Name, *listed.Entries[0].Entry.ParentID = "后续源修改", 999
							}
							listed.NextPosition.ID = 0
							return signedText, nil
						},
					)
					result, err := paging.List(ctx, actor, query, cursor)
					if err != nil || result == nil || result.Entries == nil || !reflect.DeepEqual(result.Entries, expected) || !reflect.DeepEqual(position, originalPosition) {
						t.Fatal("名称分页结果、字段副本或输入保护错误", err)
					}
					wantedCalls := []string{"list"}
					if wantVerify {
						wantedCalls = append([]string{"verify"}, wantedCalls...)
					}
					if wantSign {
						wantedCalls = append(wantedCalls, "sign")
					}
					if !reflect.DeepEqual(calls, wantedCalls) || (result.NextCursor != nil) != wantSign || (wantSign && *result.NextCursor != signedText) {
						t.Fatal("名称调用顺序或续页文本错误", calls)
					}
					result.Entries = append(result.Entries, domain.NameEntryCandidate{})
					if wantSign && *result.NextCursor != signedText {
						t.Fatal("名称游标随条目切片变化")
					}
				})
			}
		})
	}
}

func TestNameEntryPageServiceFailures(t *testing.T) {
	fixtureFailure := errors.New("fixture failure")
	for _, scope := range []string{"all", "starred"} {
		t.Run(scope, func(t *testing.T) {
			for _, mode := range []string{
				"verify error", "nil verify", "zero verify id", "empty verify key", "large verify key", "list error", "nil list", "large list", "missing next", "unexpected next",
				"wrong owner", "inactive", "root result", "zero parent", "self parent", "zero id", "zero revision", "empty name", "invalid name", "bad type", "directory size",
				"empty key", "large key", "descending key", "descending tie id", "duplicate id different key", "entry repeats bound", "entry lower key", "entry lower tie id",
				"zero next", "empty next key", "large next key", "next lower key", "next lower tie id", "empty next no progress", "empty next lower key",
				"sign error", "empty signature", "large signature", "large UTF-8 signature", "bad modified time", "modified precision",
				"cancel verify", "cancel failed verify", "cancel list", "cancel failed list", "cancel sign", "cancel failed sign",
			} {
				t.Run(mode, func(t *testing.T) {
					// 1. 模拟各层失败，任何失败都必须整体返回空结果。
					actor, query, position, listed := nameEntryPageFixture()
					query.Scope = scope
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					want := error(domain.ErrInvalidFileEntryData)
					switch mode {
					case "verify error":
						want = domain.ErrInvalidNameEntryCursor
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
						listed.Entries[0].Entry.UserID++
					case "inactive":
						listed.Entries[0].Entry.Status = "TRASHED"
					case "root result":
						listed.Entries[0].Entry.ParentID = nil
					case "zero parent":
						listed.Entries[0].Entry.ParentID = new(uint64)
					case "self parent":
						listed.Entries[0].Entry.ParentID = &listed.Entries[0].Entry.ID
					case "zero id":
						listed.Entries[0].Entry.ID = 0
					case "zero revision":
						listed.Entries[0].Entry.Revision = 0
					case "empty name":
						listed.Entries[0].Entry.Name = ""
					case "invalid name":
						listed.Entries[0].Entry.Name = string([]byte{0xff})
					case "bad type":
						listed.Entries[0].Entry.EntryType = "LINK"
					case "directory size":
						listed.Entries[0].Entry.EntryType = "DIR"
					case "empty key":
						listed.Entries[0].NameKey = nil
					case "large key":
						listed.Entries[0].NameKey = bytes.Repeat([]byte{0xff}, 769)
					case "descending key":
						listed.Entries[1].NameKey = []byte("a")
					case "descending tie id":
						listed.Entries[0], listed.Entries[1] = listed.Entries[1], listed.Entries[0]
					case "duplicate id different key":
						listed.Entries[1].Entry.ID, listed.Entries[1].NameKey = listed.Entries[0].Entry.ID, []byte("z")
					case "entry repeats bound":
						listed.Entries[0].Entry.ID = position.ID
					case "entry lower key":
						listed.Entries[0].NameKey = []byte("a")
					case "entry lower tie id":
						listed.Entries[0].Entry.ID = position.ID - 1
					case "zero next":
						listed.NextPosition.ID = 0
					case "empty next key":
						listed.NextPosition.NameKey = nil
					case "large next key":
						listed.NextPosition.NameKey = bytes.Repeat([]byte{0xff}, 769)
					case "next lower key":
						listed.NextPosition.NameKey = []byte("a")
					case "next lower tie id":
						listed.NextPosition.ID = listed.Entries[0].Entry.ID
					case "empty next no progress":
						listed.Entries, listed.NextPosition = nil, copyNameEntryPosition(position)
					case "empty next lower key":
						listed.Entries, listed.NextPosition.NameKey = nil, []byte("a")
					case "bad modified time":
						listed.Entries[0].Entry.ModifiedAt = time.Time{}
					case "modified precision":
						listed.Entries[0].Entry.ModifiedAt = listed.Entries[0].Entry.ModifiedAt.Add(time.Nanosecond)
					case "empty signature", "large signature", "large UTF-8 signature":
						want = nil
					case "cancel verify", "cancel failed verify", "cancel list", "cancel failed list", "cancel sign", "cancel failed sign":
						want = context.Canceled
					}
					verifyCalls, listCalls, signCalls := 0, 0, 0
					paging := NewNameEntryPageService(
						func(uint64, domain.GlobalEntryQuery, string) (*domain.NameEntryPosition, error) {
							verifyCalls++
							switch mode {
							case "verify error":
								return nil, domain.ErrInvalidNameEntryCursor
							case "nil verify":
								return nil, nil
							case "zero verify id":
								position.ID = 0
							case "empty verify key":
								position.NameKey = nil
							case "large verify key":
								position.NameKey = bytes.Repeat([]byte{0xff}, 769)
							case "cancel verify":
								cancel()
							case "cancel failed verify":
								cancel()
								return nil, fixtureFailure
							}
							return position, nil
						},
						func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.NameEntryPosition) (*domain.NameEntryList, error) {
							listCalls++
							if mode == "cancel list" || mode == "cancel failed list" {
								cancel()
							}
							if mode == "list error" || mode == "cancel failed list" {
								return nil, fixtureFailure
							}
							return listed, nil
						},
						func(uint64, domain.GlobalEntryQuery, domain.NameEntryPosition) (string, error) {
							signCalls++
							if mode == "cancel sign" || mode == "cancel failed sign" {
								cancel()
							}
							if mode == "sign error" || mode == "cancel failed sign" {
								return "", fixtureFailure
							}
							switch mode {
							case "empty signature":
								return "", nil
							case "large signature":
								return strings.Repeat("a", domain.MaxNameEntryCursorLength+1), nil
							case "large UTF-8 signature":
								return strings.Repeat("界", domain.MaxNameEntryCursorLength/3+1), nil
							}
							return "fixture-next", nil
						},
					)
					result, err := paging.List(ctx, actor, query, "fixture-current")
					if result != nil || err == nil || (want != nil && !errors.Is(err, want)) || verifyCalls != 1 {
						t.Fatal("名称失败未整体返回空结果", err)
					}
					wantList, wantSign := 1, 0
					if strings.Contains(mode, "verify") {
						wantList = 0
					}
					if strings.Contains(mode, "sign") {
						wantSign = 1
					}
					if listCalls != wantList || signCalls != wantSign {
						t.Fatal("名称前置失败仍调用下游", listCalls, signCalls)
					}
				})
			}
		})
	}
}

func TestNameEntryPageServiceInputs(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero page", "large page", "directory scope", "recent scope", "modified sort", "size sort", "invalid search", "invalid kind", "large cursor", "large UTF-8 cursor", "cancelled", "expired"} {
		t.Run(mode, func(t *testing.T) {
			actor, query, _, _ := nameEntryPageFixture()
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
			case "directory scope":
				query.Scope = "directory"
			case "recent scope":
				query.Scope, query.Sort = "recent", domain.GlobalEntrySortLastOpenedDesc
			case "modified sort":
				query.Sort = domain.GlobalEntrySortModifiedDesc
			case "size sort":
				query.Sort = domain.GlobalEntrySortSizeDesc
			case "invalid search":
				query.Search = string([]byte{0xff})
			case "invalid kind":
				query.Kind = "private-kind"
			case "large cursor":
				cursor, want = strings.Repeat("a", domain.MaxNameEntryCursorLength+1), domain.ErrInvalidNameEntryCursor
			case "large UTF-8 cursor":
				cursor, want = strings.Repeat("界", domain.MaxNameEntryCursorLength/3+1), domain.ErrInvalidNameEntryCursor
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
			paging := NewNameEntryPageService(
				func(uint64, domain.GlobalEntryQuery, string) (*domain.NameEntryPosition, error) {
					calls++
					return nil, nil
				},
				func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.NameEntryPosition) (*domain.NameEntryList, error) {
					calls++
					return nil, nil
				},
				func(uint64, domain.GlobalEntryQuery, domain.NameEntryPosition) (string, error) {
					calls++
					return "", nil
				},
			)
			if result, err := paging.List(ctx, actor, query, cursor); result != nil || err == nil || calls != 0 || (want != nil && !errors.Is(err, want)) {
				t.Fatal("名称非法输入调用依赖或错误不符", err)
			}
		})
	}
}

func TestNameEntryPageServiceDependencies(t *testing.T) {
	for _, missing := range []string{"verify", "list", "sign"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("名称分页缺少依赖未拒绝")
				}
			}()
			verify := func(uint64, domain.GlobalEntryQuery, string) (*domain.NameEntryPosition, error) { return nil, nil }
			list := func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.NameEntryPosition) (*domain.NameEntryList, error) {
				return nil, nil
			}
			sign := func(uint64, domain.GlobalEntryQuery, domain.NameEntryPosition) (string, error) { return "", nil }
			switch missing {
			case "verify":
				verify = nil
			case "list":
				list = nil
			case "sign":
				sign = nil
			}
			NewNameEntryPageService(verify, list, sign)
		})
	}
}
