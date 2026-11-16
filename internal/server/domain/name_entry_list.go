package domain

// NameEntryList 保存一次有界扫描取得的名称升序可见项及内部续页位置。
type NameEntryList struct {
	// Entries 按名称键及 ID 升序排列，保留每项独立的名称键。
	// 空结果使用初始化后的切片。
	Entries []NameEntryCandidate
	// HasMore 表示当前位置后还有候选可以继续检查。
	HasMore bool
	// NextPosition 在 HasMore 时保存最后消费的候选，允许 Entries 为空。
	NextPosition *NameEntryPosition
}
