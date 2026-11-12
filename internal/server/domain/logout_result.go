package domain

// LogoutResult 保存当前会话撤销事务确认提交后的内部结果。
type LogoutResult struct {
	// Revoked 为 true 表示本次会话撤销已确认提交。
	Revoked bool
}
