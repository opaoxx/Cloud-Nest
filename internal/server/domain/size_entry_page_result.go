package domain

// SizeEntryPageResult 保存带文本游标的大小降序可见分页结果。
type SizeEntryPageResult struct {
	// Entries 保存独立可见目录项，空页使用初始化后的切片。
	Entries []FileEntry
	// NextCursor 保存下一页签名文本，预算空页也可以携带续页游标。
	NextCursor *string
}
