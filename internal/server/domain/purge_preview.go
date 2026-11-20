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

// IsValid 检查可用于预检统计和摘要的目录项字段。
func (entry PurgePreviewSubtreeEntry) IsValid() bool {
	// 1. 核对标识、版本及目录大小约束。
	if entry.ID == 0 || entry.Revision == 0 ||
		(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
		return false
	}

	// 2. 只接受数据库约定的生命周期状态。
	switch entry.Status {
	case "ACTIVE", "TRASHED", "PURGING", "PURGED":
		return true
	default:
		return false
	}
}

// PurgePreviewSummary 保存尚未完成清理的数量和预计释放的逻辑用量。
type PurgePreviewSummary struct {
	// FileCount 是仍待处理的文件数。
	FileCount int
	// DirectoryCount 是仍待处理的目录数。
	DirectoryCount int
	// LogicalBytes 是每个仍待处理文件的逻辑字节数之和。
	LogicalBytes uint64
}
