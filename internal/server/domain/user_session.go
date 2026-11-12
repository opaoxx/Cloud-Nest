package domain

import "time"

// UserSession 保存用户会话列表的一条内部记录及设备展示信息。
type UserSession struct {
	// ID 是认证会话的完整无符号主键。
	ID uint64
	// UserID 是本条会话所属用户。
	UserID uint64
	// DeviceID 是会话所属设备的主键。
	DeviceID uint64
	// DeviceName 是设备当前保存的展示名称。
	DeviceName string
	// DevicePlatform 是设备当前保存的平台值。
	DevicePlatform string
	// CreatedAt 是会话创建时间。
	CreatedAt time.Time
	// ExpiresAt 是会话到期时间。
	ExpiresAt time.Time
	// LastSeenAt 保留可空的会话最近使用时间。
	LastSeenAt *time.Time
	// RevokedAt 保留可空的会话撤销时间。
	RevokedAt *time.Time
	// DeviceRevokedAt 保留可空的设备撤销时间。
	DeviceRevokedAt *time.Time
}

// UserSessionPage 保存当前批次的会话以及后续页提示。
type UserSessionPage struct {
	// Sessions 按会话 ID 降序排列，空结果使用已初始化的空切片。
	Sessions []UserSession
	// HasMore 表示本次读取时，本页之后存在其他记录。
	HasMore bool
}
