package dto

import (
	"net/url"
	"strings"
	"testing"
)

func TestParseTrashPageQuery(t *testing.T) {
	// 341 个三字节字符加一个 ASCII 字符，解码后正好为 1024 字节。
	maximumCursor := strings.Repeat("界", 341) + "a"
	for _, testCase := range []struct {
		name     string
		rawQuery string
		expected TrashPageQuery
	}{
		{name: "defaults", expected: TrashPageQuery{PageSize: 50}},
		{name: "explicit first page", rawQuery: "cursor=&limit=20", expected: TrashPageQuery{PageSize: 20}},
		{name: "bare cursor", rawQuery: "cursor", expected: TrashPageQuery{PageSize: 50}},
		{name: "minimum page size", rawQuery: "limit=1", expected: TrashPageQuery{PageSize: 1}},
		{name: "maximum page size", rawQuery: "limit=100", expected: TrashPageQuery{PageSize: 100}},
		{name: "decode once", rawQuery: "%63ursor=+A%2BB%252E%20&%6Cimit=%32", expected: TrashPageQuery{Cursor: " A+B%2E ", PageSize: 2}},
		{name: "opaque bytes", rawQuery: "cursor=%FF", expected: TrashPageQuery{Cursor: "\xff", PageSize: 50}},
		{name: "maximum decoded bytes", rawQuery: "cursor=" + url.QueryEscape(maximumCursor), expected: TrashPageQuery{Cursor: maximumCursor, PageSize: 50}},
		{name: "ignore extra fields", rawQuery: "user_id=99&parent_id=7&sort=name_asc&note=one&note=two&limit=3", expected: TrashPageQuery{PageSize: 3}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			query, fieldError := ParseTrashPageQuery(testCase.rawQuery)
			if fieldError != nil || query == nil || *query != testCase.expected {
				t.Fatalf("解析结果为 %+v，字段错误为 %+v，期望 %+v", query, fieldError, testCase.expected)
			}
		})
	}
}

func TestParseTrashPageQueryFailures(t *testing.T) {
	messages := map[string]string{
		"query":  "查询参数格式不正确",
		"cursor": "回收站游标须为单个不超过 1024 字节的文本",
		"limit":  "每页数量须为单个 1～100 的标准十进制整数",
	}
	for _, testCase := range []struct {
		name     string
		rawQuery string
		field    string
	}{
		{name: "repeated cursor", rawQuery: "cursor=one&cursor=one", field: "cursor"},
		{name: "encoded duplicate cursor", rawQuery: "cursor=&%63ursor=", field: "cursor"},
		{name: "one byte over cursor limit", rawQuery: "cursor=" + url.QueryEscape(strings.Repeat("界", 341)+"ab"), field: "cursor"},
		{name: "empty page size", rawQuery: "limit=", field: "limit"},
		{name: "zero page size", rawQuery: "limit=0", field: "limit"},
		{name: "page size above limit", rawQuery: "limit=101", field: "limit"},
		{name: "leading zero", rawQuery: "limit=01", field: "limit"},
		{name: "signed page size", rawQuery: "limit=%2B1", field: "limit"},
		{name: "space in page size", rawQuery: "limit=+1", field: "limit"},
		{name: "noninteger page size", rawQuery: "limit=1.5", field: "limit"},
		{name: "repeated page size", rawQuery: "limit=1&limit=2", field: "limit"},
		{name: "encoded duplicate page size", rawQuery: "limit=1&%6Cimit=1", field: "limit"},
		{name: "malformed cursor encoding", rawQuery: "cursor=%ZZ", field: "query"},
		{name: "unescaped semicolon", rawQuery: "cursor=one;limit=2", field: "query"},
		{name: "query error before field errors", rawQuery: "cursor=one&cursor=two&limit=0&note=%ZZ", field: "query"},
		{name: "cursor error before limit error", rawQuery: "limit=0&cursor=one&cursor=two", field: "cursor"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			query, fieldError := ParseTrashPageQuery(testCase.rawQuery)
			if query != nil || fieldError == nil || fieldError.Field != testCase.field || fieldError.Message != messages[testCase.field] {
				t.Fatalf("解析结果为 %+v，字段错误为 %+v，期望 %s 字段错误", query, fieldError, testCase.field)
			}
		})
	}
}
