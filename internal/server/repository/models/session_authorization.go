package models

import "time"

// SessionAuthorization 是会话、设备与用户 JOIN 查询的七列只读投影。
type SessionAuthorization struct {
	// UserID 对应会话所属用户。
	UserID uint64 `gorm:"column:user_id;->"`
	// SessionID 对应查询中的 session_id 别名。
	SessionID uint64 `gorm:"column:session_id;->"`
	// DeviceID 对应会话所属设备。
	DeviceID uint64 `gorm:"column:device_id;->"`
	// UserStatus 对应用户状态别名。
	UserStatus string `gorm:"column:user_status;->"`
	// SessionExpiresAt 对应会话到期时间别名。
	SessionExpiresAt time.Time `gorm:"column:session_expires_at;->"`
	// SessionRevokedAt 对应可空的会话撤销时间别名。
	SessionRevokedAt *time.Time `gorm:"column:session_revoked_at;->"`
	// DeviceRevokedAt 对应可空的设备撤销时间别名。
	DeviceRevokedAt *time.Time `gorm:"column:device_revoked_at;->"`
}
