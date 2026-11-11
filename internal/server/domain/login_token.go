package domain

// LoginTokenResult 保存会话已提交且访问令牌签发成功后的内部登录结果。
type LoginTokenResult struct {
	// Device 是本次登录使用的设备快照。
	Device LoginDevice
	// Session 是已提交的会话及刷新凭证截止时间。
	Session LoginSession
	// AccessToken 包含访问令牌原文及其截止时间，原文由 HTTP 层明确交付。
	AccessToken AccessToken
	// RefreshToken 是刷新凭证原文，后续按客户端模式通过 DTO 或 Cookie 交付。
	RefreshToken string `json:"-"`
}
