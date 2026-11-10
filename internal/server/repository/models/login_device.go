package models

import "time"

// LoginDevice 是 user_devices 的六列只读投影，不包含设备写入规则。
type LoginDevice struct {
	// ID 对应 user_devices.id。
	ID uint64 `gorm:"column:id;primaryKey;->"`
	// UserID 对应 user_devices.user_id。
	UserID uint64 `gorm:"column:user_id;->"`
	// InstallationID 对应 user_devices.installation_id。
	InstallationID string `gorm:"column:installation_id;->"`
	// Name 对应 user_devices.name。
	Name string `gorm:"column:name;->"`
	// Platform 对应 user_devices.platform。
	Platform string `gorm:"column:platform;->"`
	// RevokedAt 对应可空的 user_devices.revoked_at，不使用软删除字段类型。
	RevokedAt *time.Time `gorm:"column:revoked_at;->"`
}

// TableName 指向已有设备表，避免默认推导为 login_devices。
func (LoginDevice) TableName() string { return "user_devices" }
