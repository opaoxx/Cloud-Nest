package dto

import (
	"net/url"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// DirectoryPageQuery 保存当前普通目录列表的类型化查询参数。
type DirectoryPageQuery struct {
	// ParentID 是已解码的非零真实父目录 ID。
	ParentID uint64
	// Cursor 原样保留单个游标文本，空字符串表示首页。
	Cursor string
	// PageSize 默认 50，范围为 1～100。
	PageSize int
}

// ParseDirectoryPageQuery 解析完整 RawQuery，失败返回固定公开字段错误。
// 当前支持 directory/modified_desc 和空 q/kind，其他额外字段沿用忽略策略。
// 游标签名及用户/目录绑定由后续分页服务检查。
func ParseDirectoryPageQuery(rawQuery string) (*DirectoryPageQuery, *FieldError) {
	// 1. 完整解析查询字符串，核对当前支持的查询条件。
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &FieldError{Field: "query", Message: "查询参数格式不正确"}
	}
	for _, rule := range []struct{ field, expected, message string }{
		{field: "scope", expected: "directory", message: "当前目录查询范围须为单个 directory"},
		{field: "sort", expected: "modified_desc", message: "当前目录排序须为单个 modified_desc"},
		{field: "q", expected: "", message: "当前目录搜索条件须为单个空值"},
		{field: "kind", expected: "", message: "当前目录类型筛选须为单个空值"},
	} {
		if supplied, present := values[rule.field]; present && (len(supplied) != 1 || supplied[0] != rule.expected) {
			return nil, &FieldError{Field: rule.field, Message: rule.message}
		}
	}

	// 2. 校验并转换必填的父目录 ID。
	parentValues := values["parent_id"]
	parentError := &FieldError{Field: "parent_id", Message: "父目录 ID 须为单个 1～18446744073709551615 的标准十进制整数"}
	if len(parentValues) != 1 {
		return nil, parentError
	}
	parentID, valid := parsePathID(parentValues[0])
	if !valid {
		return nil, parentError
	}

	// 3. 检查单个游标文本，复用页大小校验。
	query := &DirectoryPageQuery{ParentID: parentID}
	if cursorValues, present := values["cursor"]; present {
		if len(cursorValues) != 1 || len(cursorValues[0]) > domain.MaxDirectoryCursorLength {
			return nil, &FieldError{Field: "cursor", Message: "目录游标须为单个不超过 1024 字节的文本"}
		}
		query.Cursor = cursorValues[0]
	}
	pageSize, fieldError := parseListPageSize(values)
	if fieldError != nil {
		return nil, fieldError
	}
	query.PageSize = pageSize

	// 4. 返回目录分页服务所需的类型化参数。
	return query, nil
}
