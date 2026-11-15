package models

import "time"

// FileEntryDisplay 是指定目录项扩展展示查询的九列只读投影。
type FileEntryDisplay struct {
	// ID 对应 file_entries.id。
	ID uint64 `gorm:"column:id;->"`
	// UserID 对应目录项所属用户。
	UserID uint64 `gorm:"column:user_id;->"`
	// Category 对应可空文件类别。
	Category *string `gorm:"column:category;->"`
	// Extension 对应可空扩展名提示。
	Extension *string `gorm:"column:extension;->"`
	// MIMEType 对应可空内容 MIME。
	MIMEType *string `gorm:"column:mime_type;->"`
	// SourceDeviceID 对应可空来源设备标识。
	SourceDeviceID *uint64 `gorm:"column:source_device_id;->"`
	// SourceDeviceName 对应发布时保存的设备名称快照。
	SourceDeviceName *string `gorm:"column:source_device_name;->"`
	// ColorKey 对应可空文件夹颜色键。
	ColorKey *string `gorm:"column:color_key;->"`
	// ContentModifiedAt 对应可空内容发布时间。
	ContentModifiedAt *time.Time `gorm:"column:content_modified_at;->"`
}

// TableName 指向已有逻辑文件与目录表。
func (FileEntryDisplay) TableName() string { return "file_entries" }
