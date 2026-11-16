package dto

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestParseNameEntryPageQuery(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, search, kind, cursor, scope string
		size                                   int
	}{
		{name: "starred defaults", raw: "scope=starred", scope: "starred", size: 50},
		{name: "starred complete", raw: "scope=starred&sort=name_asc&q=学习&kind=document&cursor=opaque.page&limit=2", scope: "starred", search: "学习", kind: "document", cursor: "opaque.page", size: 2},
		{name: "encoded starred scope", raw: "%73cope=%73tarred", scope: "starred", size: 50},
		{name: "starred whitespace", raw: "scope=starred&q=+%09%20&kind=folder", scope: "starred", search: " \t ", kind: "folder", size: 50},
		{name: "starred maximum cursor", raw: "scope=starred&limit=100&cursor=" + strings.Repeat("a", 2048), scope: "starred", cursor: strings.Repeat("a", 2048), size: 100},
		{name: "starred ignored extras", raw: "scope=starred&parent_id=private-parent&parent_id=0&user_id=99", scope: "starred", size: 50},
		{name: "starred unicode form", raw: "scope=starred&q=Cafe%CC%81&kind=image", scope: "starred", search: "Cafe\u0301", kind: "image", size: 50},
		{name: "starred literal search", raw: "scope=starred&q=" + url.QueryEscape("%_+=\x00"), scope: "starred", search: "%_+=\x00", size: 50},
		{name: "starred long search", raw: "scope=starred&q=" + url.QueryEscape(strings.Repeat("学", 2000)), scope: "starred", search: strings.Repeat("学", 2000), size: 50},
		{name: "defaults", size: 50},
		{name: "explicit defaults", raw: "scope=all&sort=name_asc&q=&kind=&cursor=&limit=50", size: 50},
		{name: "empty without equals", raw: "q&kind&cursor", size: 50},
		{name: "encoded keys and values", raw: "%73cope=%61ll&%73ort=name_asc&%71=%E5%AD%A6%E4%B9%A0%25_&%6Bind=document&%63ursor=A%2EB&%6Cimit=%32", search: "学习%_", kind: "document", cursor: "A.B", size: 2},
		{name: "minimum page", raw: "limit=1", size: 1},
		{name: "maximum page", raw: "limit=100", size: 100},
		{name: "search whitespace", raw: "q=+%09%20", search: " \t ", size: 50},
		{name: "literal characters", raw: "q=" + url.QueryEscape("%_='\\\";&+\x00"), search: "%_='\\\";&+\x00", size: 50},
		{name: "composed search", raw: "q=Caf%C3%A9", search: "Café", size: 50},
		{name: "decomposed search", raw: "q=Cafe%CC%81", search: "Cafe\u0301", size: 50},
		{name: "long search", raw: "q=" + url.QueryEscape(strings.Repeat("学", 2000)), search: strings.Repeat("学", 2000), size: 50},
		{name: "opaque cursor", raw: "cursor=%20A%2BB%20", cursor: " A+B ", size: 50},
		{name: "long cursor", raw: "cursor=" + strings.Repeat("a", 1025), cursor: strings.Repeat("a", 1025), size: 50},
		{name: "maximum cursor", raw: "cursor=" + strings.Repeat("a", 2048), cursor: strings.Repeat("a", 2048), size: 50},
		{name: "multibyte cursor", raw: "cursor=" + url.QueryEscape(strings.Repeat("界", 682)), cursor: strings.Repeat("界", 682), size: 50},
		{name: "exact multibyte cursor", raw: "cursor=" + url.QueryEscape(strings.Repeat("界", 682)+"aa"), cursor: strings.Repeat("界", 682) + "aa", size: 50},
		{name: "binary cursor", raw: "cursor=%FF%00", cursor: string([]byte{0xff, 0}), size: 50},
		{name: "identity and parent extras", raw: "parent_id=private-parent&user_id=99&device_id=private-device&before_id=private-position", size: 50},
		{name: "duplicate extras", raw: "note=private-one&note=private-two", size: 50},
		{name: "case sensitive keys", raw: "SCOPE=recent&SORT=name_asc&Q=private-search&KIND=private-kind&LIMIT=1", size: 50},
		{name: "empty separators", raw: "&&scope=all&&", size: 50},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseNameEntryPageQuery(testCase.raw)
			scope := testCase.scope
			if scope == "" {
				scope = domain.GlobalEntryScopeAll
			}
			expected := domain.GlobalEntryQuery{Scope: scope, Sort: "name_asc", Search: testCase.search, Kind: testCase.kind, PageSize: testCase.size}
			if fieldError != nil || input == nil || input.Query != expected || input.Cursor != testCase.cursor || !input.Query.IsValid() {
				t.Fatal("名称查询参数、默认值或原文错误", fieldError)
			}
		})
	}
	for _, kind := range []string{"", "folder", "document", "image", "video", "audio", "archive", "other"} {
		t.Run("kind "+kind, func(t *testing.T) {
			input, fieldError := ParseNameEntryPageQuery("kind=" + kind)
			if fieldError != nil || input == nil || input.Query.Kind != kind {
				t.Fatal("领域分类未被完整接收", fieldError)
			}
			starred, fieldError := ParseNameEntryPageQuery("scope=starred&kind=" + kind)
			if fieldError != nil || starred == nil || starred.Query.Scope != domain.GlobalEntryScopeStarred || starred.Query.Kind != kind {
				t.Fatal("收藏分类或范围未被完整接收", fieldError)
			}
		})
	}
}

func TestNameEntryPageQueryServiceParameters(t *testing.T) {
	for _, scope := range []string{"all", "starred"} {
		for _, mode := range []string{"binary position", "long position"} {
			t.Run(scope+" "+mode, func(t *testing.T) {
				// 1. 用正式名称签名器准备原始二进制位置，经 URL 和 DTO 还原查询。
				masterKey := bytes.Repeat([]byte{9}, 32)
				signer, err := service.NewNameEntryCursorSigner(masterKey)
				if err != nil {
					t.Fatal(err)
				}
				verifier, err := service.NewNameEntryCursorVerifier(masterKey)
				if err != nil {
					t.Fatal(err)
				}
				query := domain.GlobalEntryQuery{Scope: scope, Sort: "name_asc", Search: " 学习%_+ ", Kind: "document", PageSize: 50}
				position := domain.NameEntryPosition{NameKey: []byte{'a', 0, 0xff}, ID: 25}
				entry := directoryPageDataFixture().Entries[0]
				entry.ModifiedAt = entry.ModifiedAt.UTC().Truncate(time.Microsecond)
				key := []byte{'b', 0, 0xff}
				if mode == "long position" {
					position.NameKey, position.ID = bytes.Repeat([]byte{0xff}, 768), ^uint64(0)-1
					entry.ID, key = ^uint64(0), bytes.Clone(position.NameKey)
				}
				cursor, err := signer.Sign(7, query, position)
				if err != nil || (mode == "long position" && len(cursor) <= 1024) {
					t.Fatal("名称查询位置签名或长文本边界错误", err)
				}
				values := url.Values{"scope": {scope}, "q": {query.Search}, "kind": {query.Kind}, "limit": {"2"}, "cursor": {cursor}, "user_id": {"99"}}
				input, fieldError := ParseNameEntryPageQuery(values.Encode())
				if fieldError != nil || input == nil || input.Cursor != cursor {
					t.Fatal("名称查询 DTO 未保留实际游标", fieldError)
				}

				// 2. 用可信身份调用正式分页组合，核对排序、筛选和完整名称位置。
				ctx := context.Background()
				actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
				listCalls := 0
				query.PageSize = 2
				pages := service.NewNameEntryPageService(verifier.Verify, func(received context.Context, identity domain.Actor, receivedQuery domain.GlobalEntryQuery, after *domain.NameEntryPosition) (*domain.NameEntryList, error) {
					listCalls++
					if received != ctx || identity != actor || receivedQuery != query || after == nil || !reflect.DeepEqual(*after, position) {
						t.Error("名称查询条件、身份或位置传递错误")
					}
					return &domain.NameEntryList{Entries: []domain.NameEntryCandidate{{Entry: entry, NameKey: key}}}, nil
				}, signer.Sign)
				result, err := pages.List(ctx, actor, input.Query, input.Cursor)
				if err != nil || result == nil || listCalls != 1 {
					t.Fatal("名称查询 DTO 未衔接正式分页服务", err)
				}
				data, err := NewNameEntryPageData(result)
				if err != nil || data == nil || len(data.Items) != 1 || data.Items[0].ID != strconv.FormatUint(entry.ID, 10) || len(data.Breadcrumbs) != 0 || data.NextCursor != nil {
					t.Fatal("名称查询服务结果未转换为公开响应", err)
				}

				// 3. 合法格式继续交给服务核对绑定，错误文本或身份在读取前失败。
				for _, change := range []string{"search", "kind", "scope", "tampered cursor", "invalid UTF-8 cursor", "foreign user"} {
					changed := url.Values{"scope": {scope}, "q": {query.Search}, "kind": {query.Kind}, "cursor": {cursor}}
					identity := actor
					switch change {
					case "search":
						changed.Set("q", strings.TrimSpace(query.Search))
					case "kind":
						changed.Set("kind", "image")
					case "scope":
						otherScope := "all"
						if scope == "all" {
							otherScope = "starred"
						}
						changed.Set("scope", otherScope)
					case "tampered cursor":
						changed.Set("cursor", cursor+"=")
					case "invalid UTF-8 cursor":
						changed.Set("cursor", string([]byte{0xff}))
					case "foreign user":
						identity.UserID = 3
					}
					parsed, fieldError := ParseNameEntryPageQuery(changed.Encode())
					if fieldError != nil || parsed == nil {
						t.Fatal("有效格式未通过名称查询解析", fieldError)
					}
					if result, err := pages.List(ctx, identity, parsed.Query, parsed.Cursor); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) || listCalls != 1 {
						t.Fatal("名称异常绑定或游标未在读取前拒绝", err)
					}
				}
			})
		}
	}
}

func TestParseNameEntryPageQueryFailures(t *testing.T) {
	messages := map[string]string{
		"query":  "查询参数格式不正确",
		"scope":  "当前名称查询范围须为单个 all 或 starred",
		"sort":   "当前名称排序须为单个 name_asc",
		"q":      "搜索条件须为单个有效 UTF-8 文本",
		"kind":   "类型筛选须为单个空值或 folder/document/image/video/audio/archive/other",
		"cursor": "名称游标须为单个不超过 2048 字节的文本",
		"limit":  "每页数量须为单个 1～100 的标准十进制整数",
	}
	for _, testCase := range []struct{ name, raw, field string }{
		{"duplicate starred", "scope=starred&scope=starred", "scope"},
		{"encoded duplicate starred", "scope=starred&%73cope=starred", "scope"},
		{"mixed scopes", "scope=all&scope=starred", "scope"},
		{"spaced starred", "scope=starred+", "scope"},
		{"double encoded starred", "scope=%2573tarred", "scope"},
		{"starred wrong sort", "scope=starred&sort=modified_desc", "sort"},
		{"starred invalid search", "scope=starred&q=%FF", "q"},
		{"starred duplicate search", "scope=starred&q=&%71=", "q"},
		{"starred invalid kind", "scope=starred&kind=private-kind", "kind"},
		{"starred duplicate cursor", "scope=starred&cursor=&cursor=", "cursor"},
		{"starred invalid limit", "scope=starred&limit=0", "limit"},
		{"starred malformed query", "scope=starred&note=%ZZ", "query"},
		{"directory scope", "scope=directory", "scope"},
		{"uppercase starred scope", "scope=STARRED", "scope"},
		{"recent scope", "scope=recent", "scope"},
		{"empty scope", "scope=", "scope"},
		{"uppercase scope", "scope=ALL", "scope"},
		{"spaced scope", "scope=all+", "scope"},
		{"duplicate scope", "scope=all&scope=all", "scope"},
		{"encoded duplicate scope", "scope=all&%73cope=all", "scope"},
		{"modified sort", "sort=modified_desc", "sort"},
		{"size sort", "sort=size_desc", "sort"},
		{"recent sort", "sort=last_opened_desc", "sort"},
		{"empty sort", "sort=", "sort"},
		{"uppercase sort", "sort=NAME_ASC", "sort"},
		{"duplicate sort", "sort=name_asc&sort=name_asc", "sort"},
		{"duplicate search", "q=private-one&q=private-two", "q"},
		{"encoded duplicate search", "q=&%71=", "q"},
		{"invalid utf8 search", "q=%FF", "q"},
		{"truncated utf8 search", "q=%E5%AD", "q"},
		{"invalid raw utf8 search", "q=" + string([]byte{0xff}), "q"},
		{"unknown kind", "kind=private-kind", "kind"},
		{"uppercase kind", "kind=DOCUMENT", "kind"},
		{"spaced kind", "kind=folder+", "kind"},
		{"invalid utf8 kind", "kind=%FF", "kind"},
		{"duplicate kind", "kind=&kind=", "kind"},
		{"encoded duplicate kind", "kind=folder&%6Bind=folder", "kind"},
		{"duplicate cursor", "cursor=private-one&cursor=private-two", "cursor"},
		{"encoded duplicate cursor", "cursor=&%63ursor=", "cursor"},
		{"long cursor", "cursor=" + strings.Repeat("a", 2049), "cursor"},
		{"long multibyte cursor", "cursor=" + url.QueryEscape(strings.Repeat("界", 683)), "cursor"},
		{"empty limit", "limit=", "limit"},
		{"limit without equals", "limit", "limit"},
		{"zero limit", "limit=0", "limit"},
		{"negative limit", "limit=-1", "limit"},
		{"large limit", "limit=101", "limit"},
		{"leading zero limit", "limit=050", "limit"},
		{"signed limit", "limit=%2B50", "limit"},
		{"spaced limit", "limit=+50", "limit"},
		{"decimal limit", "limit=50.0", "limit"},
		{"exponent limit", "limit=5e1", "limit"},
		{"fullwidth limit", "limit=５０", "limit"},
		{"overflow limit", "limit=18446744073709551616", "limit"},
		{"duplicate limit", "limit=1&limit=2", "limit"},
		{"encoded duplicate limit", "limit=1&%6Cimit=1", "limit"},
		{"malformed query", "note=private-%ZZ", "query"},
		{"semicolon query", "scope=all;limit=1", "query"},
		{"parse priority", "scope=private-scope&q=%FF&note=%ZZ", "query"},
		{"scope priority", "scope=private-scope&sort=private-sort&q=%FF", "scope"},
		{"sort priority", "sort=private-sort&q=%FF&kind=private-kind", "sort"},
		{"search priority", "q=%FF&kind=private-kind", "q"},
		{"kind priority", "kind=private-kind&cursor=a&cursor=b&limit=private-limit", "kind"},
		{"cursor priority", "cursor=a&cursor=b&limit=private-limit", "cursor"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseNameEntryPageQuery(testCase.raw)
			if input != nil || fieldError == nil || fieldError.Field != testCase.field || fieldError.Message != messages[testCase.field] || strings.Contains(fieldError.Message, "private-") {
				t.Fatal("字段、固定提示、错误优先级或空结果错误", fieldError)
			}
		})
	}
	first, _ := ParseNameEntryPageQuery("q=学习&kind=document")
	first.Query.Search = "modified"
	second, _ := ParseNameEntryPageQuery("q=学习&kind=document")
	if first == second || second.Query.Search != "学习" {
		t.Fatal("重复解析共享查询结果")
	}
	_, firstError := ParseNameEntryPageQuery("kind=private-kind")
	firstError.Message = "modified"
	_, secondError := ParseNameEntryPageQuery("kind=private-kind")
	if firstError == secondError || secondError.Message != messages["kind"] {
		t.Fatal("重复解析共享错误指针")
	}
}
