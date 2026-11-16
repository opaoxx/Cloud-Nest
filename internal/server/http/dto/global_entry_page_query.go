package dto

import (
	"net/url"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// GlobalEntryPageQuery 保存全局列表的业务查询条件及文本游标。
type GlobalEntryPageQuery struct {
	// Query 使用领域查询模型，用户归属由认证 Actor 提供。
	Query domain.GlobalEntryQuery
	// Cursor 保留解码后的游标原文，空字符串表示首页。
	Cursor string
}

// ParseGlobalEntryPageQuery 解析完整 RawQuery，失败返回固定公开字段错误。
// 当前支持 all/modified_desc，额外字段沿用忽略策略，游标签名由分页服务校验。
func ParseGlobalEntryPageQuery(rawQuery string) (*GlobalEntryPageQuery, *FieldError) {
	// 1. 完整解析查询字符串，设置全局查询默认值。
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &FieldError{Field: "query", Message: "查询参数格式不正确"}
	}
	query := &GlobalEntryPageQuery{Query: domain.GlobalEntryQuery{
		Scope: domain.GlobalEntryScopeAll, Sort: domain.GlobalEntrySortModifiedDesc, PageSize: 50,
	}}

	// 2. 校验当前支持的范围和排序。
	for _, rule := range []struct{ field, expected, message string }{
		{field: "scope", expected: query.Query.Scope, message: "当前全局查询范围须为单个 all"},
		{field: "sort", expected: query.Query.Sort, message: "当前全局排序须为单个 modified_desc"},
	} {
		if supplied, present := values[rule.field]; present && (len(supplied) != 1 || supplied[0] != rule.expected) {
			return nil, &FieldError{Field: rule.field, Message: rule.message}
		}
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
		if len(cursorValues) != 1 || len(cursorValues[0]) > domain.MaxGlobalEntryCursorLength {
			return nil, &FieldError{Field: "cursor", Message: "全局游标须为单个不超过 1024 字节的文本"}
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
