package dto

import (
	"net/url"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RecentEntryPageQuery 保存最近使用列表的业务查询条件及文本游标。
type RecentEntryPageQuery struct {
	// Query 使用领域查询模型，用户归属由认证 Actor 提供。
	Query domain.GlobalEntryQuery
	// Cursor 保留解码后的游标原文，空字符串表示首页。
	Cursor string
}

// ParseRecentEntryPageQuery 解析完整 RawQuery，失败返回固定公开字段错误。
// 接收 recent 的访问时间降序查询，额外字段沿用忽略策略，游标签名由分页服务校验。
func ParseRecentEntryPageQuery(rawQuery string) (*RecentEntryPageQuery, *FieldError) {
	// 1. 完整解析查询字符串，设置最近使用查询默认值。
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &FieldError{Field: "query", Message: "查询参数格式不正确"}
	}
	query := &RecentEntryPageQuery{Query: domain.GlobalEntryQuery{
		Scope: domain.GlobalEntryScopeRecent, Sort: domain.GlobalEntrySortLastOpenedDesc, PageSize: 50,
	}}

	// 2. 校验最近使用范围和访问时间排序。
	if scopes, present := values["scope"]; present {
		if len(scopes) != 1 || scopes[0] != domain.GlobalEntryScopeRecent {
			return nil, &FieldError{Field: "scope", Message: "当前最近使用查询范围须为单个 recent"}
		}
		query.Query.Scope = scopes[0]
	}
	if sorts, present := values["sort"]; present && (len(sorts) != 1 || sorts[0] != domain.GlobalEntrySortLastOpenedDesc) {
		return nil, &FieldError{Field: "sort", Message: "当前最近使用排序须为单个 last_opened_desc"}
	}

	// 3. 校验搜索与分类，保留搜索原文。
	if searchValues, present := values["q"]; present {
		if len(searchValues) != 1 || !utf8.ValidString(searchValues[0]) {
			return nil, &FieldError{Field: "q", Message: "搜索条件须为单个有效 UTF-8 文本"}
		}
		query.Query.Search = searchValues[0]
	}
	if kindValues, present := values["kind"]; present {
		if len(kindValues) != 1 {
			return nil, &FieldError{Field: "kind", Message: "类型筛选须为单个空值或 folder/document/image/video/audio/archive/other"}
		}
		query.Query.Kind = kindValues[0]
	}
	if !query.Query.IsValid() {
		return nil, &FieldError{Field: "kind", Message: "类型筛选须为单个空值或 folder/document/image/video/audio/archive/other"}
	}

	// 4. 检查单个游标，复用页大小校验。
	if cursorValues, present := values["cursor"]; present {
		if len(cursorValues) != 1 || len(cursorValues[0]) > domain.MaxRecentEntryCursorLength {
			return nil, &FieldError{Field: "cursor", Message: "最近使用游标须为单个不超过 1024 字节的文本"}
		}
		query.Cursor = cursorValues[0]
	}
	pageSize, fieldError := parseListPageSize(values)
	if fieldError != nil {
		return nil, fieldError
	}
	query.Query.PageSize = pageSize

	// 5. 返回业务查询条件和游标。
	return query, nil
}
