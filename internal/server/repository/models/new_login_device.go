package models

// NewLoginDevice 只映射创建设备所需的列，其余列沿用数据库默认值。
// 与 LoginDevice 只读投影分开，防止写入撤销时间或覆盖已有设备。
type NewLoginDevice struct {
	// ID 由 MySQL 自增产生，供 Create 回填，不接受客户端指定。
	ID uint64 `gorm:"column:id;primaryKey;autoIncrement"`
	// UserID 来自登录事务已经锁定并复查的用户。
	UserID uint64 `gorm:"column:user_id;<-:create"`
	// InstallationID 是已通过登录字段校验的安装 UUID。
	InstallationID string `gorm:"column:installation_id;<-:create"`
	// Name 是已校验的设备名称，写入时保留原文。
	Name string `gorm:"column:name;<-:create"`
	// Platform 是已校验的 WEB、WINDOWS 或 ANDROID。
	Platform string `gorm:"column:platform;<-:create"`
}

// TableName 明确写入已有的 user_devices 表，不创建新表。
func (NewLoginDevice) TableName() string { return "user_devices" }
