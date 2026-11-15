package domain

import "time"

// EntryPreference 保存指定目录项的收藏及最近访问事实。
type EntryPreference struct {
	// UserID 是目录项及偏好所属用户。
	UserID uint64
	// EntryID 是文件或目录的完整无符号主键。
	EntryID uint64
	// StarredAt 为 nil 时表示当前未收藏。
	StarredAt *time.Time
	// LastOpenedAt 为 nil 时表示尚无最近成功授权访问记录。
	LastOpenedAt *time.Time
}
