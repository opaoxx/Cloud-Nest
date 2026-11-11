package domain

// LoginSessionResult 保存登录用户事务提交后供登录结果组装使用的数据。
type LoginSessionResult struct {
	// Device 是本次登录创建、复用或恢复后的设备。
	Device LoginDevice
	// Session 是已提交的登录会话，包含刷新凭证的截止时间。
	Session LoginSession
	// RefreshToken 是只在登录结果交付时使用的敏感原文。
	RefreshToken string `json:"-"`
}
