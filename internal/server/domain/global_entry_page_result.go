package domain

// GlobalEntryPageResult 保存带文本游标的全局可见分页结果。
type GlobalEntryPageResult struct {
	// Entries 是本页独立可见项，空页使用初始化后的切片。
	Entries []FileEntry
	// NextCursor 在后续候选存在时保存签名文本，允许预算空页携带续页游标。
	NextCursor *string
}
