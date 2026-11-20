package dto

import (
	"net/url"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// TrashPageQuery 保存回收站列表的类型化分页参数。
type TrashPageQuery struct {
	// Cursor 保留解码后的游标原文，空字符串表示首页。
	Cursor string
	// PageSize 默认 50，范围为 1～100。
	PageSize int
}

// ParseTrashPageQuery 解析回收站列表的 RawQuery。
// 忽略编码合法的额外参数，游标签名及用户绑定由分页服务校验。
func ParseTrashPageQuery(rawQuery string) (*TrashPageQuery, *FieldError) {
	// 1. 解析完整查询字符串，遇到非法编码就返回 query 字段错误。
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &FieldError{Field: "query", Message: "查询参数格式不正确"}
	}

	// 2. 校验游标的数量和解码后的字节数，保留游标原文。
	var cursor string
	if cursorValues, present := values["cursor"]; present {
		if len(cursorValues) != 1 || len(cursorValues[0]) > domain.MaxTrashEntryCursorLength {
			return nil, &FieldError{Field: "cursor", Message: "回收站游标须为单个不超过 1024 字节的文本"}
		}
		cursor = cursorValues[0]
	}

	// 3. 复用页大小校验，返回类型化的分页参数。
	pageSize, fieldError := parseListPageSize(values)
	if fieldError != nil {
		return nil, fieldError
	}
	return &TrashPageQuery{Cursor: cursor, PageSize: pageSize}, nil
}
