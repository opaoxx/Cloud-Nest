// Package models 定义数据库记录及只读查询投影，与 HTTP 请求 DTO 分开。
package models

// LoginAccount 是 users 表三个登录字段的只读投影，不代表完整的用户持久化模型。
type LoginAccount struct {
	// ID 对应 users.id。
	ID uint64 `gorm:"column:id;primaryKey;->"`
	// PasswordHash 对应 users.password_hash，只用于内部密码验证。
	PasswordHash string `gorm:"column:password_hash;->" json:"-"`
	// Status 对应 users.status。
	Status string `gorm:"column:status;->"`
}

// TableName 明确投影来自既有 users 表，避免按结构体名称推导错误的表名。
func (LoginAccount) TableName() string { return "users" }
