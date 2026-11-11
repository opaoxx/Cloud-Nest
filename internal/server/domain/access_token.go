package domain

import (
	"errors"
	"time"
)

// ErrInvalidAccessToken 表示访问令牌的签名、格式或声明校验失败。
var ErrInvalidAccessToken = errors.New("访问令牌无效")

// ErrAccessTokenExpired 表示签名及其他声明合格，但访问令牌已经到期。
var ErrAccessTokenExpired = errors.New("访问令牌已过期")

// AccessToken 保存签发结果，HTTP 层通过响应 DTO 明确交付令牌。
type AccessToken struct {
	// Token 是已签名的 JWT 文本，按敏感凭证处理。
	Token string `json:"-"`
	// ExpiresAt 是 JWT exp 对应的 UTC 整秒截止时间。
	ExpiresAt time.Time
}

// VerifiedAccessToken 保存签名和声明校验后供数据库授权查询使用的标识。
type VerifiedAccessToken struct {
	// UserID 是从规范 sub 字符串还原的用户主键。
	UserID uint64
	// SessionID 是从规范 sid 字符串还原的会话主键。
	SessionID uint64
	// ExpiresAt 是令牌声明的 UTC 截止时间。
	ExpiresAt time.Time
}
