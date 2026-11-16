package domain

// SizeEntryList 保存大小降序扫描取得的可见项和内部续页位置。
type SizeEntryList struct {
	// Entries 按大小和 ID 降序排列，空页使用初始化后的切片。
	Entries []FileEntry
	// HasMore 表示当前位置之后还有候选可以继续检查。
	HasMore bool
	// NextPosition 在 HasMore 时指向最后消费的候选，允许 Entries 为空。
	NextPosition *SizeEntryPosition
}
