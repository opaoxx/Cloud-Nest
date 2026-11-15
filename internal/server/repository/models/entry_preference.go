package models

import "time"

// EntryPreference 是目录项与偏好左连接查询的四列只读投影。
type EntryPreference struct {
	// UserID 对应目录项所属用户别名。
	UserID uint64 `gorm:"column:user_id;->"`
	// EntryID 对应目录项主键别名。
	EntryID uint64 `gorm:"column:entry_id;->"`
	// StarredAt 对应可空收藏时间，偏好记录缺失时同样为 nil。
	StarredAt *time.Time `gorm:"column:starred_at;->"`
	// LastOpenedAt 对应可空最近成功授权访问时间。
	LastOpenedAt *time.Time `gorm:"column:last_opened_at;->"`
}
