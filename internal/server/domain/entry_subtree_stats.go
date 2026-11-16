package domain

// EntrySubtreeStats 保存同一条查询得到的物理子树统计，包含源自身及各生命周期保留记录。
type EntrySubtreeStats struct {
	// UserID 是子树所属用户。
	UserID uint64
	// EntryID 是统计起点的源目录项标识。
	EntryID uint64
	// NodeCount 包含源自身，存在的文件或空目录均至少为一。
	NodeCount uint64
	// MaxRelativeDepth 是从源起算的最大父链边数，源自身为零。
	MaxRelativeDepth uint64
}
