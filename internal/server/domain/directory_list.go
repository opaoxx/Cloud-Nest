package domain

import "errors"

// ErrFileEntryNotDirectory 表示可见的目标是文件，无法用作目录列表的父目录。
var ErrFileEntryNotDirectory = errors.New("目标目录项不是文件夹")

// DirectoryList 保存同一快照内的可见目录路径与活动子项。
type DirectoryList struct {
	// Path 从真实根目录排列到本次读取的父目录。
	Path FileEntryPath
	// Entries 是修改时间、ID 降序排列的独立子项记录。
	Entries []FileEntry
	// HasMore 表示当前页之后还有其他活动子项。
	HasMore bool
}
