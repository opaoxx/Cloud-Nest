package domain

// TrashEntryPageResult 保存文本游标分页得到的可见回收项。
type TrashEntryPageResult struct {
	// Entries 按回收时间和 ID 双降序排列，并保留原父路径。
	Entries []TrashEntryListItem
	// NextCursor 在候选耗尽时为空。扫描达到上限时，空页也可以携带续页游标。
	NextCursor *string
}
