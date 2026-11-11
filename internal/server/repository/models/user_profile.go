package models

// UserProfile 是用户与根目录关联查询的只读投影。
type UserProfile struct {
	// ID 对应 users.id。
	ID uint64 `gorm:"column:id;->"`
	// Username 对应 users.username。
	Username string `gorm:"column:username;->"`
	// DisplayName 对应 users.display_name。
	DisplayName string `gorm:"column:display_name;->"`
	// Status 对应 users.status。
	Status string `gorm:"column:status;->"`
	// RootEntryID 对应活动根目录 id，LEFT JOIN 未找到时为 nil。
	RootEntryID *uint64 `gorm:"column:root_entry_id;->"`
	// QuotaBytes 对应 users.storage_quota。
	QuotaBytes uint64 `gorm:"column:quota_bytes;->"`
	// UsedBytes 对应 users.used_storage。
	UsedBytes uint64 `gorm:"column:used_bytes;->"`
	// ReservedBytes 对应 users.reserved_storage。
	ReservedBytes uint64 `gorm:"column:reserved_bytes;->"`
}
