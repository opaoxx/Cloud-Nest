package service

import (
	"math"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SummarizePurgePreviewSubtree 汇总已读取的清理范围内仍待处理的逻辑目录项。
// entries 包含回收目标和所有保留后代。失败返回空汇总。
func SummarizePurgePreviewSubtree(entries []domain.PurgePreviewSubtreeEntry) (domain.PurgePreviewSummary, error) {
	// 1. 校验子树范围和每条记录。
	if len(entries) == 0 {
		return domain.PurgePreviewSummary{}, domain.ErrInvalidFileEntryData
	}
	if len(entries) > domain.MaxFileEntrySubtreeNodes {
		return domain.PurgePreviewSummary{}, domain.ErrFileEntrySubtreeTooLarge
	}

	var summary domain.PurgePreviewSummary
	seen := make(map[uint64]struct{}, len(entries))
	for _, entry := range entries {
		if !entry.IsValid() {
			return domain.PurgePreviewSummary{}, domain.ErrInvalidFileEntryData
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return domain.PurgePreviewSummary{}, domain.ErrInvalidFileEntryData
		}
		seen[entry.ID] = struct{}{}

		// 2. 跳过已完成清理的记录，累计文件数和目录数。
		if entry.Status == "PURGED" {
			continue
		}
		if entry.EntryType == "DIR" {
			summary.DirectoryCount++
			continue
		}
		summary.FileCount++

		// 3. 每次累加文件大小前检查剩余空间，避免无符号整数回绕。
		if entry.FileSize > math.MaxUint64-summary.LogicalBytes {
			return domain.PurgePreviewSummary{}, domain.ErrInvalidFileEntryData
		}
		summary.LogicalBytes += entry.FileSize
	}
	// 预检目标仍在回收站，因此至少会有一个待清理项。
	if summary.FileCount+summary.DirectoryCount == 0 {
		return domain.PurgePreviewSummary{}, domain.ErrInvalidFileEntryData
	}
	return summary, nil
}
