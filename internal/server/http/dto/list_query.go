package dto

import (
	"net/url"
	"strconv"
)

// listQuery 保存两类列表共用的分页值，供公开查询 DTO 组装结果。
type listQuery struct {
	beforeID uint64
	pageSize int
}

// parseListQuery 校验共用分页格式，边界错误使用调用方提供的固定提示。
func parseListQuery(rawQuery, boundaryMessage string) (*listQuery, *FieldError) {
	// 1. 完整解析查询字符串，初始化首页边界。
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &FieldError{Field: "query", Message: "查询参数格式不正确"}
	}
	query := &listQuery{}

	// 2. 检查并转换单个 ID 边界。
	if beforeValues, present := values["before_id"]; present {
		fieldError := &FieldError{Field: "before_id", Message: boundaryMessage}
		if len(beforeValues) != 1 {
			return nil, fieldError
		}
		beforeID, err := strconv.ParseUint(beforeValues[0], 10, 64)
		if err != nil || strconv.FormatUint(beforeID, 10) != beforeValues[0] {
			return nil, fieldError
		}
		query.beforeID = beforeID
	}

	// 3. 复用单个页大小的默认值及格式校验。
	pageSize, fieldError := parseListPageSize(values)
	if fieldError != nil {
		return nil, fieldError
	}
	query.pageSize = pageSize

	// 4. 返回供公开查询 DTO 使用的分页值。
	return query, nil
}

// parseListPageSize 校验单个 limit，供列表查询共同使用。
func parseListPageSize(values url.Values) (int, *FieldError) {
	// 1. 省略时使用默认页大小，显式值须为标准十进制整数。
	if sizeValues, present := values["limit"]; present {
		fieldError := &FieldError{Field: "limit", Message: "每页数量须为单个 1～100 的标准十进制整数"}
		if len(sizeValues) != 1 {
			return 0, fieldError
		}
		pageSize, err := strconv.ParseUint(sizeValues[0], 10, 64)
		if err != nil || pageSize < 1 || pageSize > 100 || strconv.FormatUint(pageSize, 10) != sizeValues[0] {
			return 0, fieldError
		}
		return int(pageSize), nil
	}
	return 50, nil
}
