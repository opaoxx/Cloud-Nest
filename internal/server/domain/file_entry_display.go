package domain

import "time"

// FileEntryDisplay 保存指定目录项的扩展展示事实，供详情读取与基础记录组合。
type FileEntryDisplay struct {
	// ID 是文件或目录的完整无符号主键。
	ID uint64
	// UserID 是目录项所属用户。
	UserID uint64
	// Category 是文件类别，数据库为 NULL 时保留 nil。
	Category *string
	// Extension 是数据库保存的扩展名提示，nil 与空字符串分别保留。
	Extension *string
	// MIMEType 是内容发布时保存的 MIME，尚未提供时为 nil。
	MIMEType *string
	// SourceDeviceID 是内容来源设备标识，尚未提供时为 nil。
	SourceDeviceID *uint64
	// SourceDeviceName 是内容发布时的设备名称快照。
	SourceDeviceName *string
	// ColorKey 是数据库保存的文件夹颜色键，尚未提供时为 nil。
	ColorKey *string
	// ContentModifiedAt 是最近内容发布时间，尚未提供时为 nil。
	ContentModifiedAt *time.Time
}
