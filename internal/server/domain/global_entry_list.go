package domain

// GlobalEntryList 保存一次全局扫描取得的可见项及内部续页位置。
type GlobalEntryList struct {
	// Entries 按候选顺序排列，空结果使用初始化后的切片。
	Entries []FileEntry
	// HasMore 表示当前位置后还有候选可继续检查。
	HasMore bool
	// NextPosition 在 HasMore 时指向最后消费的候选，允许 Entries 为空。
	NextPosition *FileEntryPosition
}
