package dto

import (
	"net/url"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SizeEntryPageQuery 保存大小降序列表的业务查询条件及文本游标。
type SizeEntryPageQuery struct {
	// Query 使用领域查询模型，用户归属由认证 Actor 提供。
	Query domain.GlobalEntryQuery
	// Cursor 保留解码后的游标原文，空字符串表示首页。
	Cursor string
}

// ParseSizeEntryPageQuery 解析完整 RawQuery，失败返回固定公开字段错误。
// 当前支持 all/starred 的大小降序查询，额外字段沿用忽略策略，游标签名由分页服务校验。
func ParseSizeEntryPageQuery(rawQuery string) (*SizeEntryPageQuery, *FieldError) {
	// 1. 完整解析查询字符串，设置大小查询默认值。
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &FieldError{Field: "query", Message: "查询参数格式不正确"}
	}
	query := &SizeEntryPageQuery{Query: domain.GlobalEntryQuery{
		Scope: domain.GlobalEntryScopeAll, Sort: domain.GlobalEntrySortSizeDesc, PageSize: 50,
	}}

	// 2. 校验范围和大小排序，保留 all/starred 实际范围。
	if scopes, present := values["scope"]; present {
		if len(scopes) != 1 || (scopes[0] != domain.GlobalEntryScopeAll && scopes[0] != domain.GlobalEntryScopeStarred) {
			return nil, &FieldError{Field: "scope", Message: "当前大小查询范围须为单个 all 或 starred"}
		}
		query.Query.Scope = scopes[0]
	}
	if sorts, present := values["sort"]; present && (len(sorts) != 1 || sorts[0] != domain.GlobalEntrySortSizeDesc) {
		return nil, &FieldError{Field: "sort", Message: "当前大小排序须为单个 size_desc"}
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

	// 4. 检查单个大小游标，复用页大小校验。
	if cursorValues, present := values["cursor"]; present {
		if len(cursorValues) != 1 || len(cursorValues[0]) > domain.MaxSizeEntryCursorLength {
			return nil, &FieldError{Field: "cursor", Message: "大小游标须为单个不超过 1024 字节的文本"}
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
