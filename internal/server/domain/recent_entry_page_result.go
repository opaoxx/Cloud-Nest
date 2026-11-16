package domain

// RecentEntryPageResult 保存带文本游标的最近使用可见分页结果。
type RecentEntryPageResult struct {
	// Entries 保存本页独立可见文件及访问事实，空页使用初始化后的切片。
	Entries []RecentEntryCandidate
	// NextCursor 在后续候选存在时保存签名文本，允许预算空页继续翻页。
	NextCursor *string
}
