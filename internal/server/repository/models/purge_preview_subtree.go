package models

// PurgePreviewSubtreeRow 保存递归读取和结构检查需要的列。
type PurgePreviewSubtreeRow struct {
	ID            uint64  `gorm:"column:id;->"`
	UserID        uint64  `gorm:"column:user_id;->"`
	ParentID      *uint64 `gorm:"column:parent_id;->"`
	EntryType     string  `gorm:"column:entry_type;->"`
	Status        string  `gorm:"column:status;->"`
	Revision      uint64  `gorm:"column:revision;->"`
	FileSize      uint64  `gorm:"column:file_size;->"`
	RelativeDepth uint64  `gorm:"column:relative_depth;->"`
}
