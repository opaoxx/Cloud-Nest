package domain

import "errors"

// ErrRootEntryMove 表示真实根目录受根记录约束保护。
var ErrRootEntryMove = errors.New("根目录不能移动")

// ErrEntryMoveIntoSubtree 表示目标父目录位于源目录自身或其子树内。
var ErrEntryMoveIntoSubtree = errors.New("目标父目录不能是源目录自身或其子目录")

// EntryMovePaths 保存同一读取范围内通过移动关系检查的两条独立路径。
type EntryMovePaths struct {
	// Source 从真实根目录排列到源文件或目录。
	Source FileEntryPath
	// TargetParent 从同一真实根目录排列到目标父目录。
	TargetParent FileEntryPath
}
