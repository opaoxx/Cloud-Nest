// Package domain 定义不依赖 HTTP 框架和数据库工具的业务数据及错误。
package domain

import "errors"

// ErrAccountNotFound 表示账号查询未命中，不表示数据库故障；登录时还需统一凭证错误。
var ErrAccountNotFound = errors.New("账号不存在")

// LoginAccount 只保存登录比对所需的账号信息，不作为 HTTP 响应使用。
type LoginAccount struct {
	// ID 是 users.id 的无符号用户标识。
	ID uint64
	// PasswordHash 是已有 bcrypt 摘要，不得返回前端或写入日志。
	PasswordHash string `json:"-"`
	// Status 是账号当前状态，是否允许登录由后续 Service 判断。
	Status string
}
