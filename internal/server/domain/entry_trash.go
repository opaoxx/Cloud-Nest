package domain

import (
	"errors"
	"time"
)

// ErrRootEntryTrash 表示用户的真实根目录禁止回收。
var ErrRootEntryTrash = errors.New("根目录不能回收")

// ErrRootEntryRestore 表示用户的真实根目录禁止恢复。
var ErrRootEntryRestore = errors.New("根目录不能恢复")

// ErrRootEntryPurge 表示用户的真实根目录禁止永久清理。
var ErrRootEntryPurge = errors.New("根目录不能永久清理")

// ErrRestoreTargetUnavailable 表示真实根目录也无法容纳待恢复子树的深度。
var ErrRestoreTargetUnavailable = errors.New("恢复位置无法容纳目录层级")

// TrashedFileEntry 保存回收后的目录项及回收时间。
type TrashedFileEntry struct {
	FileEntry
	// DeletedAt 是该目录项被标记为 TRASHED 的数据库 UTC 时间。
	DeletedAt time.Time
}

// TrashEntryPage 保存回收站候选批次，供同一快照内的祖先检查使用。
type TrashEntryPage struct {
	// Entries 按回收时间和 ID 双降序排列，空页使用已初始化的切片。
	Entries []TrashedFileEntry
	// HasMore 表示批次之后还有候选。
	HasMore bool
}
