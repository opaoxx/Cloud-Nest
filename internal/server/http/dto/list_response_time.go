package dto

import "time"

// listResponseTime 将可空数据库时间复制为 UTC 微秒值，返回值标记时间是否合法。
func listResponseTime(value *time.Time) (*time.Time, bool) {
	// 1. 保留空值，复制并规范化非空时间。
	if value == nil {
		return nil, true
	}
	normalized := value.UTC().Truncate(time.Microsecond)

	// 2. 检查 UTC 年份处于数据库 DATETIME 的有效范围。
	if normalized.Year() < 1000 || normalized.Year() > 9999 {
		return nil, false
	}
	return &normalized, true
}
