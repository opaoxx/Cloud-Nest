package domain

// NameEntryCandidate 保存名称升序候选的基础记录和数据库名称键。
type NameEntryCandidate struct {
	// Entry 是活动普通目录项，完整祖先链可见性由上层检查。
	Entry FileEntry
	// NameKey 保存独立的 file_entries.name_key 原始字节，供排序及续页使用。
	NameKey []byte
}

// NameEntryPage 保存按名称键、ID 升序读取的候选页。
type NameEntryPage struct {
	// Entries 在空页时使用初始化后的空切片。
	Entries []NameEntryCandidate
	// HasMore 表示本次快照中当前页后还有候选。
	HasMore bool
}
