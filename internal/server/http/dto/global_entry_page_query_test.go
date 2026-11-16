package dto

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestParseGlobalEntryPageQuery(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, search, kind, cursor, scope string
		size                                   int
	}{
		{name: "starred defaults", raw: "scope=starred", scope: "starred", size: 50},
		{name: "starred complete", raw: "scope=starred&sort=modified_desc&q=学习&kind=document&cursor=opaque.page&limit=2", scope: "starred", search: "学习", kind: "document", cursor: "opaque.page", size: 2},
		{name: "encoded starred scope", raw: "%73cope=%73tarred", scope: "starred", size: 50},
		{name: "starred whitespace", raw: "scope=starred&q=+%09%20&kind=folder", scope: "starred", search: " \t ", kind: "folder", size: 50},
		{name: "starred maximum cursor", raw: "scope=starred&limit=100&cursor=" + strings.Repeat("a", 1024), scope: "starred", cursor: strings.Repeat("a", 1024), size: 100},
		{name: "starred ignored extras", raw: "scope=starred&parent_id=private-parent&parent_id=0&user_id=99", scope: "starred", size: 50},
		{name: "starred unicode form", raw: "scope=starred&q=Cafe%CC%81&kind=image", scope: "starred", search: "Cafe\u0301", kind: "image", size: 50},
		{name: "starred literal search", raw: "scope=starred&q=" + url.QueryEscape("%_+=\x00"), scope: "starred", search: "%_+=\x00", size: 50},
		{name: "starred long search", raw: "scope=starred&q=" + url.QueryEscape(strings.Repeat("学", 2000)), scope: "starred", search: strings.Repeat("学", 2000), size: 50},
		{name: "defaults", size: 50},
		{name: "explicit defaults", raw: "scope=all&sort=modified_desc&q=&kind=&cursor=&limit=50", size: 50},
		{name: "empty without equals", raw: "q&kind&cursor", size: 50},
		{name: "encoded keys and values", raw: "%73cope=%61ll&%73ort=modified_desc&%71=%E5%AD%A6%E4%B9%A0%25_&%6Bind=document&%63ursor=A%2EB&%6Cimit=%32", search: "学习%_", kind: "document", cursor: "A.B", size: 2},
		{name: "minimum page", raw: "limit=1", size: 1},
		{name: "maximum page", raw: "limit=100", size: 100},
		{name: "search whitespace", raw: "q=+%09%20", search: " \t ", size: 50},
		{name: "literal characters", raw: "q=" + url.QueryEscape("%_='\\\";&+\x00"), search: "%_='\\\";&+\x00", size: 50},
		{name: "composed search", raw: "q=Caf%C3%A9", search: "Café", size: 50},
		{name: "decomposed search", raw: "q=Cafe%CC%81", search: "Cafe\u0301", size: 50},
		{name: "long search", raw: "q=" + url.QueryEscape(strings.Repeat("学", 2000)), search: strings.Repeat("学", 2000), size: 50},
		{name: "opaque cursor", raw: "cursor=%20A%2BB%20", cursor: " A+B ", size: 50},
		{name: "maximum cursor", raw: "cursor=" + strings.Repeat("a", 1024), cursor: strings.Repeat("a", 1024), size: 50},
		{name: "multibyte cursor", raw: "cursor=" + url.QueryEscape(strings.Repeat("界", 341)), cursor: strings.Repeat("界", 341), size: 50},
		{name: "identity and parent extras", raw: "parent_id=private-parent&user_id=99&device_id=private-device&before_id=private-position", size: 50},
		{name: "duplicate extras", raw: "note=private-one&note=private-two", size: 50},
		{name: "case sensitive keys", raw: "SCOPE=recent&SORT=name_asc&Q=private-search&KIND=private-kind&LIMIT=1", size: 50},
		{name: "empty separators", raw: "&&scope=all&&", size: 50},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseGlobalEntryPageQuery(testCase.raw)
			scope := testCase.scope
			if scope == "" {
				scope = domain.GlobalEntryScopeAll
			}
			expected := domain.GlobalEntryQuery{Scope: scope, Sort: "modified_desc", Search: testCase.search, Kind: testCase.kind, PageSize: testCase.size}
			if fieldError != nil || input == nil || input.Query != expected || input.Cursor != testCase.cursor || !input.Query.IsValid() {
				t.Fatal("全局查询参数、默认值或原文错误", fieldError)
			}
		})
	}
	for _, kind := range []string{"", "folder", "document", "image", "video", "audio", "archive", "other"} {
		t.Run("kind "+kind, func(t *testing.T) {
			input, fieldError := ParseGlobalEntryPageQuery("kind=" + kind)
			if fieldError != nil || input == nil || input.Query.Kind != kind {
				t.Fatal("领域分类未被完整接收", fieldError)
			}
			starred, fieldError := ParseGlobalEntryPageQuery("scope=starred&kind=" + kind)
			if fieldError != nil || starred == nil || starred.Query.Scope != domain.GlobalEntryScopeStarred || starred.Query.Kind != kind {
				t.Fatal("收藏分类或范围未被完整接收", fieldError)
			}
		})
	}
}

func TestParseGlobalEntryPageQueryFailures(t *testing.T) {
	messages := map[string]string{
		"query":  "查询参数格式不正确",
		"scope":  "当前全局查询范围须为单个 all 或 starred",
		"sort":   "当前全局排序须为单个 modified_desc",
		"q":      "搜索条件须为单个有效 UTF-8 文本",
		"kind":   "类型筛选须为单个空值或 folder/document/image/video/audio/archive/other",
		"cursor": "全局游标须为单个不超过 1024 字节的文本",
		"limit":  "每页数量须为单个 1～100 的标准十进制整数",
	}
	for _, testCase := range []struct{ name, raw, field string }{
		{"duplicate starred", "scope=starred&scope=starred", "scope"},
		{"encoded duplicate starred", "scope=starred&%73cope=starred", "scope"},
		{"mixed scopes", "scope=all&scope=starred", "scope"},
		{"spaced starred", "scope=starred+", "scope"},
		{"double encoded starred", "scope=%2573tarred", "scope"},
		{"starred wrong sort", "scope=starred&sort=name_asc", "sort"},
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
		{"name sort", "sort=name_asc", "sort"},
		{"size sort", "sort=size_desc", "sort"},
		{"recent sort", "sort=last_opened_desc", "sort"},
		{"empty sort", "sort=", "sort"},
		{"uppercase sort", "sort=MODIFIED_DESC", "sort"},
		{"duplicate sort", "sort=modified_desc&sort=modified_desc", "sort"},
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
		{"long cursor", "cursor=" + strings.Repeat("a", 1025), "cursor"},
		{"long multibyte cursor", "cursor=" + url.QueryEscape(strings.Repeat("界", 342)), "cursor"},
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
			input, fieldError := ParseGlobalEntryPageQuery(testCase.raw)
			if input != nil || fieldError == nil || fieldError.Field != testCase.field || fieldError.Message != messages[testCase.field] || strings.Contains(fieldError.Message, "private-") {
				t.Fatal("字段、固定提示、错误优先级或空结果错误", fieldError)
			}
		})
	}
	first, _ := ParseGlobalEntryPageQuery("q=学习&kind=document")
	first.Query.Search = "modified"
	second, _ := ParseGlobalEntryPageQuery("q=学习&kind=document")
	if first == second || second.Query.Search != "学习" {
		t.Fatal("重复解析共享查询结果")
	}
	_, firstError := ParseGlobalEntryPageQuery("kind=private-kind")
	firstError.Message = "modified"
	_, secondError := ParseGlobalEntryPageQuery("kind=private-kind")
	if firstError == secondError || secondError.Message != messages["kind"] {
		t.Fatal("重复解析共享错误指针")
	}
}

func TestGlobalEntryPageQueryServiceParameters(t *testing.T) {
	testGlobalEntryPageQueryServiceParameters(t, domain.GlobalEntryScopeAll)
}

func TestStarredGlobalEntryPageQueryServiceParameters(t *testing.T) {
	testGlobalEntryPageQueryServiceParameters(t, domain.GlobalEntryScopeStarred)
}

func testGlobalEntryPageQueryServiceParameters(t *testing.T, scope string) {
	t.Helper()
	// 1. 使用真实签名器生成绑定搜索原文的游标，通过查询 DTO 还原输入。
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := service.NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewGlobalEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	query := domain.GlobalEntryQuery{Scope: scope, Sort: "modified_desc", Search: " 学习%_+ ", Kind: "document", PageSize: 50}
	position := domain.FileEntryPosition{ModifiedAt: time.Date(2026, 11, 15, 3, 30, 0, 123456000, time.UTC), ID: 25}
	cursor, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"scope": {scope}, "q": {query.Search}, "kind": {query.Kind}, "limit": {"2"}, "cursor": {cursor}, "user_id": {"99"}}
	input, fieldError := ParseGlobalEntryPageQuery(values.Encode())
	if fieldError != nil || input == nil {
		t.Fatal(fieldError)
	}

	// 2. 把类型化条件与可信 Actor 交给真实分页组合，确认合法页大小调整。
	ctx := context.Background()
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
	listCalls := 0
	query.PageSize = 2
	pages := service.NewGlobalEntryPageService(verifier.Verify, func(received context.Context, identity domain.Actor, receivedQuery domain.GlobalEntryQuery, before *domain.FileEntryPosition) (*domain.GlobalEntryList, error) {
		listCalls++
		if received != ctx || identity != actor || receivedQuery != query || before == nil || !reflect.DeepEqual(*before, position) {
			t.Error("类型化条件、可信身份或游标位置传递错误")
		}
		entries := directoryPageDataFixture().Entries
		entries[0].ModifiedAt = entries[0].ModifiedAt.UTC().Truncate(time.Microsecond)
		return &domain.GlobalEntryList{Entries: entries}, nil
	}, signer.Sign)
	result, err := pages.List(ctx, actor, input.Query, input.Cursor)
	if err != nil || result == nil || listCalls != 1 {
		t.Fatal("查询 DTO 未衔接真实全局分页服务", err)
	}
	data, err := NewGlobalEntryPageData(result)
	if err != nil || data == nil || len(data.Items) != 1 || data.Items[0].ID != "23" || len(data.Breadcrumbs) != 0 || data.NextCursor != nil {
		t.Fatal("服务结果未转换为全局响应", err)
	}

	// 3. 修改范围、搜索或分类后完成格式解析，再由分页服务核对原游标绑定。
	for _, field := range []string{"q", "kind", "scope"} {
		changed := url.Values{"scope": {scope}, "q": {query.Search}, "kind": {query.Kind}, "cursor": {cursor}}
		if field == "q" {
			changed.Set(field, strings.TrimSpace(query.Search))
		} else if field == "kind" {
			changed.Set(field, "image")
		} else {
			other := domain.GlobalEntryScopeAll
			if scope == domain.GlobalEntryScopeAll {
				other = domain.GlobalEntryScopeStarred
			}
			changed.Set(field, other)
		}
		parsed, fieldError := ParseGlobalEntryPageQuery(changed.Encode())
		if fieldError != nil || parsed == nil {
			t.Fatal("有效筛选未通过格式解析", fieldError)
		}
		invalid, err := pages.List(ctx, actor, parsed.Query, parsed.Cursor)
		if invalid != nil || !errors.Is(err, domain.ErrInvalidGlobalEntryCursor) || listCalls != 1 {
			t.Fatal("变化的筛选未在读取前被游标校验拒绝", err)
		}
	}
}
