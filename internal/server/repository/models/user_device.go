package models

import "time"

// UserDevice 是设备列表与名称更新查询共用的七列只读投影。
type UserDevice struct {
	// ID 对应设备主键。
	ID uint64 `gorm:"column:id;->"`
	// UserID 对应设备所属用户。
	UserID uint64 `gorm:"column:user_id;->"`
	// Name 对应设备展示名称。
	Name string `gorm:"column:name;->"`
	// Platform 对应设备平台。
	Platform string `gorm:"column:platform;->"`
	// CreatedAt 对应设备创建时间。
	CreatedAt time.Time `gorm:"column:created_at;->"`
	// LastSeenAt 对应可空的最近认证活动时间。
	LastSeenAt *time.Time `gorm:"column:last_seen_at;->"`
	// RevokedAt 对应可空的设备撤销时间。
	RevokedAt *time.Time `gorm:"column:revoked_at;->"`
}

// TableName 指向已有用户设备表。
func (UserDevice) TableName() string { return "user_devices" }
