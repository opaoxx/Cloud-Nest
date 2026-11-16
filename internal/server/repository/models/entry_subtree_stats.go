package models

// EntrySubtreeStats 保存递归子树查询的聚合投影。
type EntrySubtreeStats struct {
	// UserID 来自相对深度为零的起点记录。
	UserID uint64 `gorm:"column:user_id;->"`
	// EntryID 来自相对深度为零的起点标识。
	EntryID uint64 `gorm:"column:entry_id;->"`
	// NodeCount 是递归结果的记录数。
	NodeCount uint64 `gorm:"column:node_count;->"`
	// MaxRelativeDepth 是递归结果的最大相对深度。
	MaxRelativeDepth uint64 `gorm:"column:max_relative_depth;->"`
	// InvalidParent 表示发现文件或未知类型充当父目录。
	InvalidParent uint64 `gorm:"column:invalid_parent;->"`
}
