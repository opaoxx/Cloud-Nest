package domain

// RefreshTokenResult 保存刷新事务提交且访问令牌签发成功后的内部结果。
type RefreshTokenResult struct {
	// Session 是已完成刷新摘要轮换的会话与截止时间。
	Session LoginSession
	// TokenGeneration 是本次轮换已提交的新代次。
	TokenGeneration uint64
	// AccessToken 包含新访问令牌及其截止时间，原文由交付层明确输出。
	AccessToken AccessToken
	// RefreshToken 是新刷新原文，由后续客户端响应明确交付。
	RefreshToken string `json:"-"`
}
