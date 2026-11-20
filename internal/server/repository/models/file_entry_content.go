package models

// FileEntryContent 是目录项归属及内容引用的四列只读投影。
type FileEntryContent struct {
	// ID 对应 file_entries.id。
	ID uint64 `gorm:"column:id;->"`
	// UserID 对应 file_entries.user_id。
	UserID uint64 `gorm:"column:user_id;->"`
	// ObjectID 对应可空的 file_entries.object_id。
	ObjectID *uint64 `gorm:"column:object_id;->"`
	// MIMEType 对应可空的 file_entries.mime_type。
	MIMEType *string `gorm:"column:mime_type;->"`
}

// TableName 指向已有逻辑文件与目录表。
func (FileEntryContent) TableName() string { return "file_entries" }
