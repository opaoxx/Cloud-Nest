package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// validateEntrySubtreeStats 核对统计的来源和形状，操作限制由调用方处理。
func validateEntrySubtreeStats(source domain.FileEntry, stats *domain.EntrySubtreeStats) error {
	if stats == nil || stats.UserID != source.UserID || stats.EntryID != source.ID ||
		stats.NodeCount <= stats.MaxRelativeDepth || stats.MaxRelativeDepth > domain.MaxFileEntryDepth ||
		(stats.MaxRelativeDepth == 0 && stats.NodeCount != 1) {
		return domain.ErrInvalidFileEntryData
	}
	switch source.EntryType {
	case "FILE":
		// 文件是叶子，统计中只有它自己。
		if stats.NodeCount != 1 || stats.MaxRelativeDepth != 0 {
			return domain.ErrInvalidFileEntryData
		}
	case "DIR":
	default:
		return domain.ErrInvalidFileEntryData
	}
	return nil
}
