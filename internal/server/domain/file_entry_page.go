package domain

import "time"

// FileEntryPosition 保存修改时间降序、ID 降序分页的内部位置。
type FileEntryPosition struct {
	// ModifiedAt 是上一页末项的修改时间，精度与 MySQL DATETIME(6) 一致。
	ModifiedAt time.Time
	// ID 是上一页末项的完整无符号目录项 ID。
	ID uint64
}

// IsValid 检查非零标识、MySQL UTC 年份范围和微秒精度。
func (position FileEntryPosition) IsValid() bool {
	// 1. 按 UTC 核对数据库可表示的时间与标识。
	utcYear := position.ModifiedAt.UTC().Year()
	return position.ID != 0 && utcYear >= 1000 && utcYear <= 9999 && position.ModifiedAt.Nanosecond()%1000 == 0
}

// FileEntryPage 保存活动子项的本页数据及后续页提示。
type FileEntryPage struct {
	// Entries 按修改时间及 ID 降序排列，空页使用已初始化的空切片。
	Entries []FileEntry
	// HasMore 表示本次快照中本页之后还有其他记录。
	HasMore bool
}
