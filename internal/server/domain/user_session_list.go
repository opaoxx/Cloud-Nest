package domain

// UserSessionListItem 在会话查询记录上附加本次请求的当前会话标记。
type UserSessionListItem struct {
	// UserSession 保存会话及设备事实的独立副本。
	UserSession
	// Current 表示本条会话 ID 与已认证 Actor.SessionID 相同。
	Current bool
}

// UserSessionList 保存服务返回的本页记录及后续页提示。
type UserSessionList struct {
	// Sessions 按会话 ID 降序排列，空结果使用已初始化的空切片。
	Sessions []UserSessionListItem
	// HasMore 保留查询时的后续页提示。
	HasMore bool
}
