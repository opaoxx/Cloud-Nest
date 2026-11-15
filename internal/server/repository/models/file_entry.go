package models

import "time"

// FileEntry 是目录项基础信息与活动子项查询共用的十一列只读投影。
type FileEntry struct {
	// ID 对应 file_entries.id。
	ID uint64 `gorm:"column:id;->"`
	// UserID 对应目录项所属用户。
	UserID uint64 `gorm:"column:user_id;->"`
	// ParentID 对应可空父目录，根目录为 nil。
	ParentID *uint64 `gorm:"column:parent_id;->"`
	// Name 对应目录项展示名称。
	Name string `gorm:"column:name;->"`
	// EntryType 对应文件或目录类型。
	EntryType string `gorm:"column:entry_type;->"`
	// FileSize 对应内容大小快照。
	FileSize uint64 `gorm:"column:file_size;->"`
	// Revision 对应内容或命名空间版本。
	Revision uint64 `gorm:"column:revision;->"`
	// Status 对应目录项生命周期状态。
	Status string `gorm:"column:status;->"`
	// CreatedAt 对应创建时间。
	CreatedAt time.Time `gorm:"column:created_at;->"`
	// ModifiedAt 对应业务变更时间。
	ModifiedAt time.Time `gorm:"column:modified_at;->"`
	// UpdatedAt 对应数据库记录更新时间。
	UpdatedAt time.Time `gorm:"column:updated_at;->"`
}

// TableName 指向已有逻辑文件与目录表。
func (FileEntry) TableName() string { return "file_entries" }
