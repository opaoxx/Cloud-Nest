package domain

import "time"

// RecentEntryPosition 保存最近访问时间降序、目录项 ID 降序分页的内部位置。
// 该位置可以指向最后扫描的隐藏候选，供扫描预算耗尽后的续页使用。
type RecentEntryPosition struct {
	// LastOpenedAt 来自当前用户偏好的访问时间，精度与 MySQL DATETIME(6) 一致。
	LastOpenedAt time.Time
	// ID 是最后扫描候选的完整无符号目录项 ID，同一访问时间下按 ID 降序。
	ID uint64
}

// IsValid 检查非零标识、MySQL UTC 年份范围和微秒精度。
func (position RecentEntryPosition) IsValid() bool {
	// 1. 将最近访问时间换算为 UTC，取得数据库年份。
	utcYear := position.LastOpenedAt.UTC().Year()

	// 2. 检查非零目录项 ID、1000～9999 年范围及微秒精度。
	return position.ID != 0 && utcYear >= 1000 && utcYear <= 9999 && position.LastOpenedAt.Nanosecond()%1000 == 0
}
