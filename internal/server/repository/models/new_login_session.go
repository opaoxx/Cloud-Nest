package models

import "time"

// NewLoginSession 只映射会话创建所需的列，其他字段沿用现有数据库默认值。
type NewLoginSession struct {
	// ID 由数据库自增产生，并在插入后回填。
	ID uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	// UserID 来自当前登录用户事务。
	UserID uint64 `gorm:"column:user_id;<-:create"`
	// DeviceID 是已经锁定并检查有效的设备主键。
	DeviceID uint64 `gorm:"column:device_id;<-:create"`
	// RefreshTokenHash 保存 32 字节摘要，供 BINARY(32) 列写入。
	RefreshTokenHash []byte `gorm:"column:refresh_token_hash;<-:create" json:"-"`
	// ExpiresAt 保存 UTC 微秒精度的会话到期时间。
	ExpiresAt time.Time `gorm:"column:expires_at;<-:create"`
}

// TableName 指向已有的认证会话表。
func (NewLoginSession) TableName() string { return "auth_sessions" }
