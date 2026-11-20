package models

import "time"

// PurgeProgressTarget 是 file_entries 中清理进度目标的六列只读投影。
type PurgeProgressTarget struct {
	ID             uint64     `gorm:"column:id;->"`
	UserID         uint64     `gorm:"column:user_id;->"`
	ParentID       *uint64    `gorm:"column:parent_id;->"`
	Status         string     `gorm:"column:status;->"`
	PurgeStartedAt *time.Time `gorm:"column:purge_started_at;->"`
	PurgedAt       *time.Time `gorm:"column:purged_at;->"`
}

// TableName 指向已有逻辑文件与目录表。
func (PurgeProgressTarget) TableName() string { return "file_entries" }
