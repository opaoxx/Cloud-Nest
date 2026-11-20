package models

import "time"

// FileEntryTrash 在目录项基础读取上增加回收和清理开始时间。
type FileEntryTrash struct {
	FileEntry
	// DeletedAt 对应可空的 file_entries.deleted_at。
	DeletedAt *time.Time `gorm:"column:deleted_at;->"`
	// PurgeStartedAt 对应可空的 file_entries.purge_started_at。
	PurgeStartedAt *time.Time `gorm:"column:purge_started_at;->"`
}
