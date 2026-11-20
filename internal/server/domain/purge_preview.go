package domain

// PurgePreviewSubtreeEntry 是预检统计与摘要共用的目录项事实。
type PurgePreviewSubtreeEntry struct {
	// ID 是目录项的完整无符号标识。
	ID uint64
	// EntryType 区分文件与目录。
	EntryType string
	// Status 包含全部仍保留的生命周期状态。
	Status string
	// Revision 是快照中的当前逻辑版本。
	Revision uint64
	// FileSize 是文件的逻辑大小，目录为零。
	FileSize uint64
}
