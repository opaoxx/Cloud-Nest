package domain

// RefreshSession 保存刷新凭证定位到的会话及授权状态快照。
type RefreshSession struct {
	// SessionAuthorization 复用用户、设备、会话期限及撤销状态。
	SessionAuthorization
	// TokenGeneration 是当前刷新凭证代次，后续轮换时再次核对。
	TokenGeneration uint64
}
