package models

// RefreshSession 是按刷新摘要查询的八列只读投影。
type RefreshSession struct {
	// SessionAuthorization 嵌入已有七列授权状态映射。
	SessionAuthorization `gorm:"embedded"`
	// TokenGeneration 对应 auth_sessions.token_generation。
	TokenGeneration uint64 `gorm:"column:token_generation;->"`
}
