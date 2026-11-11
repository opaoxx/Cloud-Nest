package domain

import (
	"errors"
	"time"
)

// ErrSessionNotFound 表示指定用户下没有具备完整归属关联的会话记录。
var ErrSessionNotFound = errors.New("会话不存在")

// SessionAuthorization 保存普通请求授权判断所需的数据库状态。
type SessionAuthorization struct {
	// UserID 是会话所属用户主键。
	UserID uint64
	// SessionID 是认证会话主键。
	SessionID uint64
	// DeviceID 是会话所属设备主键。
	DeviceID uint64
	// UserStatus 是数据库保存的账号状态，由服务层判断。
	UserStatus string
	// SessionExpiresAt 是数据库中的会话到期时间。
	SessionExpiresAt time.Time
	// SessionRevokedAt 为 nil 表示会话尚未撤销。
	SessionRevokedAt *time.Time
	// DeviceRevokedAt 为 nil 表示设备尚未撤销。
	DeviceRevokedAt *time.Time
}
