package domain

import "time"

// PurgeProgressTarget 保存用于判断清理进度查询资格的目录项事实。
type PurgeProgressTarget struct {
	// ID 是待查询的目录项标识。
	ID uint64
	// UserID 是目录项所属用户。
	UserID uint64
	// ParentID 为 nil 时表示真实根目录。
	ParentID *uint64
	// Status 是目录项当前生命周期状态。
	Status string
	// PurgeStartedAt 记录目标作为清理根项的开始时间，数据库为 NULL 时为 nil。
	PurgeStartedAt *time.Time
	// PurgedAt 记录目标清理完成时间，数据库为 NULL 时为 nil。
	PurgedAt *time.Time
}
