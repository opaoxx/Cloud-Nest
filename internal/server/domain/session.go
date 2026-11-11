package domain

import (
	"errors"
	"time"
)

// ErrRefreshTokenConflict 表示刷新摘要已被其他会话使用，调用方应结束本次事务。
var ErrRefreshTokenConflict = errors.New("刷新令牌摘要冲突")

// ErrInvalidSessionData 表示会话标识无效，或写入没有得到一条有效记录和非零主键。
var ErrInvalidSessionData = errors.New("登录会话数据异常")

// ErrSessionExpired 表示会话已经到期，无法继续用于授权或令牌签发。
var ErrSessionExpired = errors.New("登录会话已到期")

// ErrSessionRevoked 表示会话已被撤销，当前请求应结束授权流程。
var ErrSessionRevoked = errors.New("登录会话已撤销")

// LoginSession 保存创建会话后供登录流程使用的身份与到期信息。
type LoginSession struct {
	// ID 是数据库生成的会话主键，后续用于 Access Token 的 sid。
	ID uint64
	// UserID 是当前事务已经锁定并复查的用户。
	UserID uint64
	// DeviceID 是会话所属的有效设备。
	DeviceID uint64
	// ExpiresAt 是 UTC 微秒精度的会话到期时间。
	ExpiresAt time.Time
}
