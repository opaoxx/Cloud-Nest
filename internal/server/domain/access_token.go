package domain

import "time"

// AccessToken 保存签发结果，HTTP 层通过响应 DTO 明确交付令牌。
type AccessToken struct {
	// Token 是已签名的 JWT 文本，按敏感凭证处理。
	Token string `json:"-"`
	// ExpiresAt 是 JWT exp 对应的 UTC 整秒截止时间。
	ExpiresAt time.Time
}
