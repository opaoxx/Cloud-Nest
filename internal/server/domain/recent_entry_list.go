package domain

// RecentEntryList 保存一次有界扫描取得的可见最近文件及内部续页位置。
type RecentEntryList struct {
	// Entries 按访问时间及 ID 降序排列，保留每个可见文件的访问时间。
	// 空结果使用初始化后的切片。
	Entries []RecentEntryCandidate
	// HasMore 表示当前位置后还有候选可继续检查。
	HasMore bool
	// NextPosition 在 HasMore 时指向最后消费的候选，允许 Entries 为空。
	NextPosition *RecentEntryPosition
}
