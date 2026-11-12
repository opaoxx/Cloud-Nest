package models

import "time"

// LockedRefreshSession 是刷新事务中按主键锁定读取的七列投影。
type LockedRefreshSession struct {
	// ID 是当前会话主键。
	ID uint64 `gorm:"column:id;primaryKey;->"`
	// UserID 是会话所属用户。
	UserID uint64 `gorm:"column:user_id;->"`
	// DeviceID 是会话所属设备。
	DeviceID uint64 `gorm:"column:device_id;->"`
	// RefreshTokenHash 是数据库中的当前刷新摘要，普通 JSON 序列化时省略。
	RefreshTokenHash []byte `gorm:"column:refresh_token_hash;->" json:"-"`
	// TokenGeneration 是数据库中的当前刷新代次。
	TokenGeneration uint64 `gorm:"column:token_generation;->"`
	// ExpiresAt 是数据库中的会话截止时间。
	ExpiresAt time.Time `gorm:"column:expires_at;->"`
	// RevokedAt 是可空的会话撤销时间。
	RevokedAt *time.Time `gorm:"column:revoked_at;->"`
}

// TableName 指向已有认证会话表。
func (LockedRefreshSession) TableName() string { return "auth_sessions" }
