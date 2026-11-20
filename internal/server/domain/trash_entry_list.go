package domain

// TrashEntryListItem 保存可见回收项与本次快照中的原父路径。
type TrashEntryListItem struct {
	// Entry 保存通过可见性检查的回收项。
	Entry TrashedFileEntry
	// OriginalPath 从真实根目录到 Entry 的原父目录。
	OriginalPath FileEntryPath
}

// TrashEntryList 保存有界扫描得到的一页可见回收项。
type TrashEntryList struct {
	// Entries 按回收时间和 ID 双降序排列，空页使用已初始化的切片。
	Entries []TrashEntryListItem
	// NextPosition 指向最后消费的候选。候选耗尽时为空，空页也可能带续读位置。
	NextPosition *TrashEntryPosition
}
