package domain

// NameEntryPageResult 保存带文本游标的名称升序可见分页结果。
type NameEntryPageResult struct {
	// Entries 保存独立的可见目录项及原始名称键，空页使用初始化后的切片。
	Entries []NameEntryCandidate
	// NextCursor 保存下一页签名文本，预算空页也可以携带续页游标。
	NextCursor *string
}
