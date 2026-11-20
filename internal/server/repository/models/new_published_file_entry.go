package models

import "time"

// NewPublishedFileEntry 映射创建发布文件目录项所需的十四列。
// 自增 ID、活动名称键、版本和其他初始字段由数据库生成或填充。
type NewPublishedFileEntry struct {
	// 1. 映射用户、父目录、展示名和名称键。
	// UserID 取自当前发布事务绑定的用户。
	UserID uint64 `gorm:"column:user_id;<-:create"`
	// ParentID 指向经过路径检查的目标目录。
	ParentID uint64 `gorm:"column:parent_id;<-:create"`
	// Name 保存选定的发布展示名。
	Name string `gorm:"column:name;<-:create"`
	// NameKey 保存统一名称规则生成的判重键。
	NameKey []byte `gorm:"column:name_key;<-:create"`

	// 2. 映射文件状态、分类及 READY 对象引用。
	// EntryType 由发布写入流程固定为 FILE。
	EntryType string `gorm:"column:entry_type;<-:create"`
	// Status 由发布写入流程固定为 ACTIVE。
	Status string `gorm:"column:status;<-:create"`
	// Category 是从发布名称派生的文件分类。
	Category string `gorm:"column:category;<-:create"`
	// Extension 是可空的小写末级扩展名。
	Extension *string `gorm:"column:extension;<-:create"`
	// MIMEType 保存根据文件内容识别的基础媒体类型，未知时写入 NULL。
	MIMEType *string `gorm:"column:mime_type;<-:create"`
	// ObjectID 引用同一用户已选定的 READY 对象。
	ObjectID uint64 `gorm:"column:object_id;<-:create"`
	// FileSize 保存对象大小快照，零字节有效。
	FileSize uint64 `gorm:"column:file_size;<-:create"`

	// 3. 映射来源设备快照和内容发布时间。
	// SourceDeviceID 对应上传任务的来源设备。
	SourceDeviceID uint64 `gorm:"column:source_device_id;<-:create"`
	// SourceDeviceName 保存发布时的设备名称原文。
	SourceDeviceName string `gorm:"column:source_device_name;<-:create"`
	// ContentModifiedAt 由发布写入流程使用数据库 UTC 微秒时间填充。
	ContentModifiedAt time.Time `gorm:"column:content_modified_at;<-:create"`
}

// TableName 指向既有逻辑文件与目录表。
func (NewPublishedFileEntry) TableName() string {
	// 1. 返回既有目录项表名。
	return "file_entries"
}
