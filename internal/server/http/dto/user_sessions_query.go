package dto

import (
	"net/url"
	"strconv"
)

// UserSessionListQuery 保存会话列表查询的类型化分页参数。
type UserSessionListQuery struct {
	// BeforeSessionID 对应 before_id，0 表示读取首页。
	BeforeSessionID uint64
	// PageSize 对应 limit，省略时为 50，合法范围为 1～100。
	PageSize int
}

// ParseUserSessionListQuery 解析原始查询字符串，失败返回固定公开字段错误。
// 识别 before_id/limit，额外字段采用忽略策略，数字按解码后的标准十进制值检查。
func ParseUserSessionListQuery(rawQuery string) (*UserSessionListQuery, *FieldError) {
	// 1. 完整解析查询字符串，设置首页和页大小默认值。
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &FieldError{Field: "query", Message: "查询参数格式不正确"}
	}
	query := &UserSessionListQuery{PageSize: 50}

	// 2. 检查并转换单个会话边界。
	if beforeValues, present := values["before_id"]; present {
		fieldError := &FieldError{Field: "before_id", Message: "会话边界须为单个 0～18446744073709551615 的标准十进制整数"}
		if len(beforeValues) != 1 {
			return nil, fieldError
		}
		beforeID, err := strconv.ParseUint(beforeValues[0], 10, 64)
		if err != nil || strconv.FormatUint(beforeID, 10) != beforeValues[0] {
			return nil, fieldError
		}
		query.BeforeSessionID = beforeID
	}

	// 3. 检查并转换单个页大小。
	if sizeValues, present := values["limit"]; present {
		fieldError := &FieldError{Field: "limit", Message: "每页数量须为单个 1～100 的标准十进制整数"}
		if len(sizeValues) != 1 {
			return nil, fieldError
		}
		pageSize, err := strconv.ParseUint(sizeValues[0], 10, 64)
		if err != nil || pageSize < 1 || pageSize > 100 || strconv.FormatUint(pageSize, 10) != sizeValues[0] {
			return nil, fieldError
		}
		query.PageSize = int(pageSize)
	}

	// 4. 返回供列表服务使用的类型化参数。
	return query, nil
}
