package models

import "time"

// UserSession 是会话列表关联设备查询的十列只读投影。
type UserSession struct {
	// ID 对应会话主键别名。
	ID uint64 `gorm:"column:id;->"`
	// UserID 对应会话所属用户。
	UserID uint64 `gorm:"column:user_id;->"`
	// DeviceID 对应会话所属设备主键。
	DeviceID uint64 `gorm:"column:device_id;->"`
	// DeviceName 对应设备名称别名。
	DeviceName string `gorm:"column:device_name;->"`
	// DevicePlatform 对应设备平台别名。
	DevicePlatform string `gorm:"column:device_platform;->"`
	// CreatedAt 对应会话创建时间。
	CreatedAt time.Time `gorm:"column:created_at;->"`
	// ExpiresAt 对应会话到期时间。
	ExpiresAt time.Time `gorm:"column:expires_at;->"`
	// LastSeenAt 对应可空的会话最近使用时间。
	LastSeenAt *time.Time `gorm:"column:last_seen_at;->"`
	// RevokedAt 对应可空的会话撤销时间。
	RevokedAt *time.Time `gorm:"column:revoked_at;->"`
	// DeviceRevokedAt 对应可空的设备撤销时间别名。
	DeviceRevokedAt *time.Time `gorm:"column:device_revoked_at;->"`
}
