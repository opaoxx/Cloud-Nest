package dto

// UserDeviceListQuery 保存设备列表查询的类型化分页参数。
type UserDeviceListQuery struct {
	// BeforeDeviceID 对应 before_id，0 表示读取首页。
	BeforeDeviceID uint64
	// PageSize 对应 limit，省略时为 50，合法范围为 1～100。
	PageSize int
}

// ParseUserDeviceListQuery 解析原始查询字符串，失败返回固定公开字段错误。
// 识别 before_id/limit，额外字段采用忽略策略，数字按解码后的标准十进制值检查。
func ParseUserDeviceListQuery(rawQuery string) (*UserDeviceListQuery, *FieldError) {
	// 1. 复用共用分页校验，使用设备边界的固定提示。
	query, fieldError := parseListQuery(rawQuery, "设备边界须为单个 0～18446744073709551615 的标准十进制整数")
	if fieldError != nil {
		return nil, fieldError
	}

	// 2. 将分页值组装为设备服务所需的类型化参数。
	return &UserDeviceListQuery{BeforeDeviceID: query.beforeID, PageSize: query.pageSize}, nil
}
