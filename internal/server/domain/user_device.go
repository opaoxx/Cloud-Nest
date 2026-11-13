package domain

import "time"

// UserDevice 保存用户设备列表的一条内部记录。
type UserDevice struct {
	// ID 是设备记录的完整无符号主键。
	ID uint64
	// UserID 是本条设备所属用户。
	UserID uint64
	// Name 是设备当前保存的展示名称。
	Name string
	// Platform 是设备当前保存的平台值。
	Platform string
	// CreatedAt 是设备创建时间。
	CreatedAt time.Time
	// LastSeenAt 保留可空的设备最近认证活动时间。
	LastSeenAt *time.Time
	// RevokedAt 保留可空的设备撤销时间。
	RevokedAt *time.Time
}

// UserDevicePage 保存当前批次设备以及后续页提示。
type UserDevicePage struct {
	// Devices 按设备 ID 降序排列，空结果使用已初始化的空切片。
	Devices []UserDevice
	// HasMore 表示本次读取时，本页之后存在其他记录。
	HasMore bool
}
