package domain

// RefreshSessionResult 保存刷新事务提交后供访问令牌签发与交付使用的结果。
type RefreshSessionResult struct {
	// Session 是本次完成摘要轮换的会话及其固定截止时间。
	Session LoginSession
	// TokenGeneration 是提交后的刷新代次。
	TokenGeneration uint64
	// RefreshToken 是仅供显式交付使用的新刷新原文。
	RefreshToken string `json:"-"`
}
