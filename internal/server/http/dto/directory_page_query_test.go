package dto

import (
	"bytes"
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestParseDirectoryPageQuery(t *testing.T) {
	for _, testCase := range []struct {
		name, raw string
		parentID  uint64
		cursor    string
		size      int
	}{
		{name: "defaults", raw: "parent_id=12", parentID: 12, size: 50},
		{name: "explicit first page", raw: "parent_id=12&cursor=", parentID: 12, size: 50},
		{name: "cursor without equals", raw: "parent_id=12&cursor", parentID: 12, size: 50},
		{name: "next page", raw: "parent_id=12&cursor=opaque.page&limit=2", parentID: 12, cursor: "opaque.page", size: 2},
		{name: "encoded names and values", raw: "parent%5Fid=%31%32&%63ursor=A%2EB&%6Cimit=%32", parentID: 12, cursor: "A.B", size: 2},
		{name: "minimum values", raw: "parent_id=1&limit=1", parentID: 1, size: 1},
		{name: "maximum values", raw: "parent_id=18446744073709551615&limit=100", parentID: ^uint64(0), size: 100},
		{name: "explicit query mode", raw: "scope=directory&sort=modified_desc&q=&kind=&parent_id=12", parentID: 12, size: 50},
		{name: "empty filters without equals", raw: "parent_id=12&q&kind", parentID: 12, size: 50},
		{name: "extra identity ignored", raw: "parent_id=12&user_id=99&device_id=private-device", parentID: 12, size: 50},
		{name: "extra repeated fields", raw: "parent_id=12&note=a&note=b&before_id=private-value", parentID: 12, size: 50},
		{name: "case sensitive optional names", raw: "parent_id=12&LIMIT=1&SCOPE=all", parentID: 12, size: 50},
		{name: "empty separators", raw: "&&parent_id=12&&", parentID: 12, size: 50},
		{name: "opaque cursor preserved", raw: "parent_id=12&cursor=%20A%2BB%20", parentID: 12, cursor: " A+B ", size: 50},
		{name: "maximum cursor bytes", raw: "parent_id=12&cursor=" + strings.Repeat("a", 1024), parentID: 12, cursor: strings.Repeat("a", 1024), size: 50},
		{name: "multibyte cursor bytes", raw: "parent_id=12&cursor=" + url.QueryEscape(strings.Repeat("界", 341)), parentID: 12, cursor: strings.Repeat("界", 341), size: 50},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseDirectoryPageQuery(testCase.raw)
			if fieldError != nil || input == nil || input.ParentID != testCase.parentID || input.Cursor != testCase.cursor || input.PageSize != testCase.size {
				t.Fatal("目录查询参数或默认值错误", fieldError)
			}
		})
	}
}

func TestParseDirectoryPageQueryFailures(t *testing.T) {
	messages := map[string]string{
		"query":     "查询参数格式不正确",
		"scope":     "当前目录查询范围须为单个 directory",
		"sort":      "当前目录排序须为单个 modified_desc",
		"q":         "当前目录搜索条件须为单个空值",
		"kind":      "当前目录类型筛选须为单个空值",
		"parent_id": "父目录 ID 须为单个 1～18446744073709551615 的标准十进制整数",
		"cursor":    "目录游标须为单个不超过 1024 字节的文本",
		"limit":     "每页数量须为单个 1～100 的标准十进制整数",
	}
	for _, testCase := range []struct{ name, raw, field string }{
		{"missing parent", "", "parent_id"},
		{"empty parent", "parent_id=", "parent_id"},
		{"parent without equals", "parent_id", "parent_id"},
		{"zero parent", "parent_id=0", "parent_id"},
		{"leading zero parent", "parent_id=012", "parent_id"},
		{"signed parent", "parent_id=%2B12", "parent_id"},
		{"negative parent", "parent_id=-12", "parent_id"},
		{"parent whitespace", "parent_id=12%20", "parent_id"},
		{"parent control", "parent_id=12%00", "parent_id"},
		{"parent decimal", "parent_id=12.0", "parent_id"},
		{"parent exponent", "parent_id=1e2", "parent_id"},
		{"parent fullwidth", "parent_id=１２", "parent_id"},
		{"parent overflow", "parent_id=18446744073709551616", "parent_id"},
		{"parent duplicate", "parent_id=12&parent_id=12", "parent_id"},
		{"encoded parent duplicate", "parent_id=12&parent%5Fid=13", "parent_id"},
		{"double encoded parent", "parent_id=%2531", "parent_id"},
		{"unknown parent name", "PARENT_ID=12", "parent_id"},
		{"duplicate cursor", "parent_id=12&cursor=a&cursor=b", "cursor"},
		{"encoded duplicate cursor", "parent_id=12&cursor=a&%63ursor=b", "cursor"},
		{"empty duplicate cursor", "parent_id=12&cursor=&cursor=", "cursor"},
		{"cursor too long", "parent_id=12&cursor=" + strings.Repeat("a", 1025), "cursor"},
		{"multibyte too long", "parent_id=12&cursor=" + url.QueryEscape(strings.Repeat("界", 342)), "cursor"},
		{"empty limit", "parent_id=12&limit=", "limit"},
		{"limit without equals", "parent_id=12&limit", "limit"},
		{"zero limit", "parent_id=12&limit=0", "limit"},
		{"negative limit", "parent_id=12&limit=-1", "limit"},
		{"large limit", "parent_id=12&limit=101", "limit"},
		{"leading zero limit", "parent_id=12&limit=050", "limit"},
		{"signed limit", "parent_id=12&limit=%2B50", "limit"},
		{"spaced limit", "parent_id=12&limit=+50", "limit"},
		{"decimal limit", "parent_id=12&limit=50.0", "limit"},
		{"limit overflow", "parent_id=12&limit=18446744073709551616", "limit"},
		{"duplicate limit", "parent_id=12&limit=1&limit=2", "limit"},
		{"encoded duplicate limit", "parent_id=12&limit=1&%6Cimit=2", "limit"},
		{"scope selection", "scope=all&parent_id=12", "scope"},
		{"empty scope", "scope=&parent_id=12", "scope"},
		{"duplicate scope", "scope=directory&scope=directory&parent_id=12", "scope"},
		{"sort selection", "sort=name_asc&parent_id=12", "sort"},
		{"empty sort", "sort=&parent_id=12", "sort"},
		{"duplicate sort", "sort=modified_desc&sort=modified_desc&parent_id=12", "sort"},
		{"search selection", "q=private-search&parent_id=12", "q"},
		{"duplicate search", "q=&q=&parent_id=12", "q"},
		{"kind selection", "kind=private-kind&parent_id=12", "kind"},
		{"duplicate kind", "kind=&kind=&parent_id=12", "kind"},
		{"malformed query", "parent_id=12&note=%ZZ", "query"},
		{"semicolon query", "parent_id=12;limit=1", "query"},
		{"parse priority", "scope=all&parent_id=private-parent&note=%ZZ", "query"},
		{"mode priority", "scope=all&parent_id=private-parent", "scope"},
		{"parent priority", "parent_id=private-parent&limit=private-limit", "parent_id"},
		{"cursor priority", "parent_id=12&cursor=a&cursor=b&limit=private-limit", "cursor"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseDirectoryPageQuery(testCase.raw)
			if input != nil || fieldError == nil || fieldError.Field != testCase.field || fieldError.Message != messages[testCase.field] || strings.Contains(fieldError.Message, "private-") {
				t.Fatal("字段定位、固定提示或错误优先级不准确", fieldError)
			}
		})
	}
	_, first := ParseDirectoryPageQuery("parent_id=private-parent")
	first.Message = "modified"
	_, second := ParseDirectoryPageQuery("parent_id=private-parent")
	if first == second || second.Message != messages["parent_id"] {
		t.Fatal("重复解析共享错误指针")
	}
}

func TestDirectoryPageQueryServiceParameters(t *testing.T) {
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := service.NewDirectoryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewDirectoryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	position := domain.FileEntryPosition{ModifiedAt: time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC), ID: 25}
	cursor, err := signer.Sign(7, 12, position)
	if err != nil {
		t.Fatal(err)
	}
	input, fieldError := ParseDirectoryPageQuery("parent_id=12&cursor=" + url.QueryEscape(cursor) + "&limit=2&user_id=99")
	if fieldError != nil || input == nil {
		t.Fatal(fieldError)
	}
	fixture := directoryPageDataFixture()
	ctx := context.Background()
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
	listCalls := 0
	pages := service.NewDirectoryPageService(verifier.Verify, func(received context.Context, identity domain.Actor, parentID uint64, before *domain.FileEntryPosition, size int) (*domain.DirectoryList, error) {
		listCalls++
		if received != ctx || identity != actor || parentID != 12 || before == nil || !reflect.DeepEqual(*before, position) || size != 2 {
			t.Error("类型化查询参数或可信 Actor 传递错误")
		}
		return &domain.DirectoryList{Path: fixture.Path, Entries: fixture.Entries}, nil
	}, signer.Sign)
	result, err := pages.List(ctx, actor, input.ParentID, input.Cursor, input.PageSize)
	if err != nil || result == nil || listCalls != 1 {
		t.Fatal("查询 DTO 未衔接到真实分页组合服务", err)
	}
	data, err := NewDirectoryPageData(result)
	if err != nil || data == nil || len(data.Items) != 1 || data.Items[0].ID != "23" || data.NextCursor != nil {
		t.Fatal("服务结果未转换为正确响应", err)
	}
}
