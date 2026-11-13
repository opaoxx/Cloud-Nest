package domain

// RevokeDeviceResult 保存设备及关联会话撤销事务确认提交后的内部结果。
type RevokeDeviceResult struct {
	// Revoked 为 true 表示目标设备及关联会话已撤销，事务已确认提交。
	Revoked bool
}
