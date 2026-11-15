package domain

import "errors"

// MaxFileEntryDepth 限定目录项距根目录的最大父链边数，根目录深度为 0。
const MaxFileEntryDepth = 32

// ErrFileEntryDepthExceeded 表示本次目录操作将超过允许的最大层级。
var ErrFileEntryDepthExceeded = errors.New("目录层级不能超过 32 层")

// FileEntryPath 保存已在同一快照中通过可见性检查的独立路径记录。
type FileEntryPath struct {
	// Entries 从真实根目录排列到目标项，中间项均为活动目录。
	Entries []FileEntry
}
