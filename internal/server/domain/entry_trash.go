package domain

import (
	"errors"
	"time"
)

// ErrRootEntryTrash 表示用户的真实根目录禁止回收。
var ErrRootEntryTrash = errors.New("根目录不能回收")

// TrashedFileEntry 保存回收后的目录项及回收时间。
type TrashedFileEntry struct {
	FileEntry
	// DeletedAt 是该目录项被标记为 TRASHED 的数据库 UTC 时间。
	DeletedAt time.Time
}
