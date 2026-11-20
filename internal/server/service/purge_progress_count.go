package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// CountPurgeRemainingEntries 统计清理中的目标自身和尚未清理的后代。
// entries 来自同一只读快照中的完整子树，读取方负责按归属和父子关系校验范围。
func CountPurgeRemainingEntries(entryID uint64, entries []domain.PurgePreviewSubtreeEntry) (int, error) {
	// 1. 检查目标 ID 和子树范围。
	if entryID == 0 || len(entries) == 0 {
		return 0, domain.ErrInvalidFileEntryData
	}
	if len(entries) > domain.MaxFileEntrySubtreeNodes {
		return 0, domain.ErrFileEntrySubtreeTooLarge
	}

	// 2. 核对每条记录与目标状态，累计尚未清理的项目。
	seenEntries := make(map[uint64]struct{}, len(entries))
	remainingCount := 0
	for _, entry := range entries {
		if !entry.IsValid() {
			return 0, domain.ErrInvalidFileEntryData
		}
		if _, duplicate := seenEntries[entry.ID]; duplicate {
			return 0, domain.ErrInvalidFileEntryData
		}
		seenEntries[entry.ID] = struct{}{}
		if entry.ID == entryID && (entry.Status != "PURGING" || (entry.EntryType == "FILE" && len(entries) != 1)) {
			return 0, domain.ErrInvalidFileEntryData
		}
		if entry.Status != "PURGED" {
			remainingCount++
		}
	}

	// 3. 确认子树包含目标自身，再交付计数。
	if _, found := seenEntries[entryID]; !found {
		return 0, domain.ErrInvalidFileEntryData
	}
	return remainingCount, nil
}
