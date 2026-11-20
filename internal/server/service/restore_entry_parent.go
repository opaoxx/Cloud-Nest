package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// SelectRestoreEntryParent 选择满足深度限制的实际恢复父目录。
// target、ancestors 和 stats 来自同一用户事务中的读取与检查。
func SelectRestoreEntryParent(target *domain.FileEntry, ancestors *RestoreEntryAncestors, stats *domain.EntrySubtreeStats) (uint64, error) {
	// 1. 核对回收目标、父链和子树统计。
	if target == nil || target.ID == 0 || target.UserID == 0 || target.Revision == 0 || target.Status != "TRASHED" ||
		target.ParentID == nil || *target.ParentID == 0 || *target.ParentID == target.ID {
		return 0, domain.ErrInvalidFileEntryData
	}
	originalParentID := *target.ParentID
	if ancestors == nil || ancestors.RootEntryID == 0 || ancestors.RootEntryID == target.ID ||
		ancestors.OriginalParentDepth >= domain.MaxFileEntryDepth {
		return 0, domain.ErrInvalidFileEntryData
	}
	originalParentIsRoot := originalParentID == ancestors.RootEntryID
	if originalParentIsRoot != (ancestors.OriginalParentDepth == 0) || (originalParentIsRoot && ancestors.HasTrashedAncestor) {
		return 0, domain.ErrInvalidFileEntryData
	}
	if err := validateEntrySubtreeStats(*target, stats); err != nil {
		return 0, err
	}

	// 2. 计算恢复到原位置时的最深层级，符合条件就选原父目录。
	// 待恢复项目位于父目录下一层，再加上它到最深后代的距离。
	depthBelowParent := 1 + stats.MaxRelativeDepth
	if !ancestors.HasTrashedAncestor && ancestors.OriginalParentDepth+depthBelowParent <= domain.MaxFileEntryDepth {
		return originalParentID, nil
	}

	// 3. 原位置不可用时检查真实根目录，超过 32 层则拒绝。
	if depthBelowParent > domain.MaxFileEntryDepth {
		return 0, domain.ErrRestoreTargetUnavailable
	}
	return ancestors.RootEntryID, nil
}
