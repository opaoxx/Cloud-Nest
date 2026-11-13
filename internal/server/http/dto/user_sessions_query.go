package dto

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
	// 1. 复用共用分页校验，使用会话边界的固定提示。
	query, fieldError := parseListQuery(rawQuery, "会话边界须为单个 0～18446744073709551615 的标准十进制整数")
	if fieldError != nil {
		return nil, fieldError
	}

	// 2. 将分页值组装为会话服务所需的类型化参数。
	return &UserSessionListQuery{BeforeSessionID: query.beforeID, PageSize: query.pageSize}, nil
}
