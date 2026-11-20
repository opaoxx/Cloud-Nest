package domain

import "time"

// TrashEntryPosition 保存回收时间降序、目录项 ID 降序分页的内部位置。
// 位置可以指向最后消费的隐藏候选，供达到扫描预算时继续读取。
type TrashEntryPosition struct {
	// DeletedAt 是候选的回收时间，精度与 MySQL DATETIME(6) 一致。
	DeletedAt time.Time
	// ID 是候选的完整无符号目录项 ID，同一回收时间下按 ID 降序。
	ID uint64
}

// IsValid 检查回收站分页位置是否可用于数据库比较。
func (position TrashEntryPosition) IsValid() bool {
	// 1. 把回收时间换算成 UTC，取得用于数据库校验的年份。
	utcYear := position.DeletedAt.UTC().Year()

	// 2. 检查目录项 ID 非零、年份在 1000～9999 内、时间对齐到微秒。
	return position.ID != 0 && utcYear >= 1000 && utcYear <= 9999 &&
		position.DeletedAt.Nanosecond()%int(time.Microsecond) == 0
}
