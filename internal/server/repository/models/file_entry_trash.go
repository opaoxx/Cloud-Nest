package models

import "time"

// FileEntryTrash 在目录项基础读取上增加回收时间，供回收事务使用。
type FileEntryTrash struct {
	FileEntry
	// DeletedAt 对应可空的 file_entries.deleted_at。
	DeletedAt *time.Time `gorm:"column:deleted_at;->"`
}
