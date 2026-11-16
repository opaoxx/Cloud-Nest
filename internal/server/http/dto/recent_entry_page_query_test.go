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

func TestParseRecentEntryPageQuery(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, search, kind, cursor string
		size                            int
	}{
		{name: "defaults", size: 50},
		{name: "explicit recent", raw: "scope=recent", size: 50},
		{name: "opaque invalid UTF-8 cursor", raw: "cursor=%FF", cursor: string([]byte{0xff}), size: 50},
		{name: "explicit defaults", raw: "scope=recent&sort=last_opened_desc&q=&kind=&cursor=&limit=50", size: 50},
		{name: "empty without equals", raw: "q&kind&cursor", size: 50},
		{name: "encoded keys and values", raw: "%73cope=%72ecent&%73ort=last_opened_desc&%71=%E5%AD%A6%E4%B9%A0%25_&%6Bind=document&%63ursor=A%2EB&%6Cimit=%32", search: "学习%_", kind: "document", cursor: "A.B", size: 2},
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
		{name: "empty separators", raw: "&&scope=recent&&", size: 50},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseRecentEntryPageQuery(testCase.raw)
			expected := domain.GlobalEntryQuery{Scope: domain.GlobalEntryScopeRecent, Sort: "last_opened_desc", Search: testCase.search, Kind: testCase.kind, PageSize: testCase.size}
			if fieldError != nil || input == nil || input.Query != expected || input.Cursor != testCase.cursor || !input.Query.IsValid() {
				t.Fatal("最近使用查询参数、默认值或原文错误", fieldError)
			}
		})
	}
	for _, kind := range []string{"", "folder", "document", "image", "video", "audio", "archive", "other"} {
		t.Run("kind "+kind, func(t *testing.T) {
			input, fieldError := ParseRecentEntryPageQuery("kind=" + kind)
			if fieldError != nil || input == nil || input.Query.Kind != kind {
				t.Fatal("领域分类未被完整接收", fieldError)
			}
		})
	}
}

func TestParseRecentEntryPageQueryFailures(t *testing.T) {
	messages := map[string]string{
		"query":  "查询参数格式不正确",
		"scope":  "当前最近使用查询范围须为单个 recent",
		"sort":   "当前最近使用排序须为单个 last_opened_desc",
		"q":      "搜索条件须为单个有效 UTF-8 文本",
		"kind":   "类型筛选须为单个空值或 folder/document/image/video/audio/archive/other",
		"cursor": "最近使用游标须为单个不超过 1024 字节的文本",
		"limit":  "每页数量须为单个 1～100 的标准十进制整数",
	}
	for _, testCase := range []struct{ name, raw, field string }{
		{"directory scope", "scope=directory", "scope"},
		{"all scope", "scope=all", "scope"},
		{"starred scope", "scope=starred", "scope"},
		{"double encoded scope", "scope=%2572ecent", "scope"},
		{"empty scope", "scope=", "scope"},
		{"uppercase scope", "scope=RECENT", "scope"},
		{"spaced scope", "scope=recent+", "scope"},
		{"duplicate scope", "scope=recent&scope=recent", "scope"},
		{"encoded duplicate scope", "scope=recent&%73cope=recent", "scope"},
		{"name sort", "sort=name_asc", "sort"},
		{"size sort", "sort=size_desc", "sort"},
		{"modified sort", "sort=modified_desc", "sort"},
		{"empty sort", "sort=", "sort"},
		{"uppercase sort", "sort=LAST_OPENED_DESC", "sort"},
		{"duplicate sort", "sort=last_opened_desc&sort=last_opened_desc", "sort"},
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
		{"semicolon query", "scope=recent;limit=1", "query"},
		{"parse priority", "scope=private-scope&q=%FF&note=%ZZ", "query"},
		{"scope priority", "scope=private-scope&sort=private-sort&q=%FF", "scope"},
		{"sort priority", "sort=private-sort&q=%FF&kind=private-kind", "sort"},
		{"search priority", "q=%FF&kind=private-kind", "q"},
		{"kind priority", "kind=private-kind&cursor=a&cursor=b&limit=private-limit", "kind"},
		{"cursor priority", "cursor=a&cursor=b&limit=private-limit", "cursor"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseRecentEntryPageQuery(testCase.raw)
			if input != nil || fieldError == nil || fieldError.Field != testCase.field || fieldError.Message != messages[testCase.field] || strings.Contains(fieldError.Message, "private-") {
				t.Fatal("字段、固定提示、错误优先级或空结果错误", fieldError)
			}
		})
	}
	first, _ := ParseRecentEntryPageQuery("q=学习&kind=document")
	first.Query.Search = "modified"
	second, _ := ParseRecentEntryPageQuery("q=学习&kind=document")
	if first == second || second.Query.Search != "学习" {
		t.Fatal("重复解析共享查询结果")
	}
	_, firstError := ParseRecentEntryPageQuery("kind=private-kind")
	firstError.Message = "modified"
	_, secondError := ParseRecentEntryPageQuery("kind=private-kind")
	if firstError == secondError || secondError.Message != messages["kind"] {
		t.Fatal("重复解析共享错误指针")
	}
}

func TestRecentEntryPageQueryServiceParameters(t *testing.T) {
	// 1. 使用正式签名器生成访问游标，经查询 DTO 保留筛选原文并调整合法页大小。
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := service.NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewRecentEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	query := domain.GlobalEntryQuery{Scope: "recent", Sort: "last_opened_desc", Search: " 学习%_+ ", Kind: "document", PageSize: 50}
	position := domain.RecentEntryPosition{LastOpenedAt: time.Date(2026, 11, 16, 3, 30, 0, 123456000, time.UTC), ID: 25}
	cursor, err := signer.Sign(7, query, position)
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"scope": {"recent"}, "q": {query.Search}, "kind": {query.Kind}, "limit": {"2"}, "cursor": {cursor}, "user_id": {"99"}}
	input, fieldError := ParseRecentEntryPageQuery(values.Encode())
	if fieldError != nil || input == nil {
		t.Fatal(fieldError)
	}

	// 2. 将类型化条件与可信 Actor 交给正式分页组合，再转换公开访问时间。
	ctx := context.Background()
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
	listCalls := 0
	query.PageSize = 2
	pages := service.NewRecentEntryPageService(verifier.Verify, func(received context.Context, identity domain.Actor, receivedQuery domain.GlobalEntryQuery, before *domain.RecentEntryPosition) (*domain.RecentEntryList, error) {
		listCalls++
		if received != ctx || identity != actor || receivedQuery != query || before == nil || !reflect.DeepEqual(*before, position) {
			t.Error("类型化条件、可信身份或访问位置传递错误")
		}
		entry := directoryPageDataFixture().Entries[0]
		entry.ModifiedAt = entry.ModifiedAt.UTC().Truncate(time.Microsecond)
		return &domain.RecentEntryList{Entries: []domain.RecentEntryCandidate{{Entry: entry, LastOpenedAt: position.LastOpenedAt.Add(-time.Hour)}}}, nil
	}, signer.Sign)
	result, err := pages.List(ctx, actor, input.Query, input.Cursor)
	if err != nil || result == nil || listCalls != 1 {
		t.Fatal("查询 DTO 未衔接最近使用分页服务", err)
	}
	data, err := NewRecentEntryPageData(result)
	if err != nil || data == nil || len(data.Items) != 1 || data.Items[0].ID != "23" || !data.Items[0].LastOpenedAt.Equal(position.LastOpenedAt.Add(-time.Hour)) || len(data.Breadcrumbs) != 0 || data.NextCursor != nil {
		t.Fatal("最近使用结果未转换为公开响应", err)
	}

	// 3. 合法筛选变化和异常游标先完成格式解析，再由服务在列表读取前拒绝。
	for _, mode := range []string{"search", "kind", "tampered cursor", "invalid UTF-8 cursor", "foreign user"} {
		changed := url.Values{"scope": {"recent"}, "q": {query.Search}, "kind": {query.Kind}, "cursor": {cursor}}
		currentActor := actor
		switch mode {
		case "search":
			changed.Set("q", strings.TrimSpace(query.Search))
		case "kind":
			changed.Set("kind", "image")
		case "tampered cursor":
			changed.Set("cursor", cursor+"=")
		case "invalid UTF-8 cursor":
			changed.Set("cursor", string([]byte{0xff}))
		case "foreign user":
			currentActor.UserID = 3
		}
		parsed, fieldError := ParseRecentEntryPageQuery(changed.Encode())
		if fieldError != nil || parsed == nil {
			t.Fatal("有效格式未通过解析", fieldError)
		}
		invalid, err := pages.List(ctx, currentActor, parsed.Query, parsed.Cursor)
		if invalid != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) || listCalls != 1 {
			t.Fatal("异常绑定或游标未在读取前拒绝", err)
		}
	}
}
