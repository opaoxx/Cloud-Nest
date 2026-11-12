package models

import "time"

// LockedSession 是已认证写事务读取的五列会话授权投影。
type LockedSession struct {
	// ID 对应 auth_sessions.id。
	ID uint64 `gorm:"column:id;primaryKey;->"`
	// UserID 是会话所属用户。
	UserID uint64 `gorm:"column:user_id;->"`
	// DeviceID 是会话所属设备。
	DeviceID uint64 `gorm:"column:device_id;->"`
	// ExpiresAt 是当前会话截止时间。
	ExpiresAt time.Time `gorm:"column:expires_at;->"`
	// RevokedAt 是可空的会话撤销时间。
	RevokedAt *time.Time `gorm:"column:revoked_at;->"`
}

// TableName 指向已有认证会话表。
func (LockedSession) TableName() string { return "auth_sessions" }
