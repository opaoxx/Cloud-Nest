package domain

import "errors"

// MaxFileEntrySubtreeNodes 限定单次目录操作的子树节点数量，包含源自身。
const MaxFileEntrySubtreeNodes = 10000

// ErrFileEntrySubtreeTooLarge 表示本次目录操作的子树超出节点数量上限。
var ErrFileEntrySubtreeTooLarge = errors.New("单次目录操作不能超过 10000 个目录项")

// EntrySubtreeStats 保存同一条查询得到的物理子树统计，包含源自身及各生命周期保留记录。
type EntrySubtreeStats struct {
	// UserID 是子树所属用户。
	UserID uint64
	// EntryID 是统计起点的源目录项标识。
	EntryID uint64
	// NodeCount 包含源自身，存在的文件或空目录均至少为一。
	NodeCount uint64
	// MaxRelativeDepth 是从源起算的最大父链边数，源自身为零。
	MaxRelativeDepth uint64
}
