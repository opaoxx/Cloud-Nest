package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// ValidateEntryMoveLimits 核对移动子树的节点数量及移动后最大层级。
// paths 先由移动关系服务校验，统计与路径应来自同一用户事务。
// 当前父链可见性、根目录和子树关系由已有服务检查，本方法保留输入原值。
func ValidateEntryMoveLimits(paths *domain.EntryMovePaths, stats *domain.EntrySubtreeStats) error {
	// 1. 检查路径输入及用户、源标识的对应关系。
	if paths == nil || len(paths.Source.Entries) < 2 || len(paths.Source.Entries) > domain.MaxFileEntryDepth+1 ||
		len(paths.TargetParent.Entries) == 0 || len(paths.TargetParent.Entries) > domain.MaxFileEntryDepth+1 {
		return domain.ErrInvalidFileEntryData
	}
	source := paths.Source.Entries[len(paths.Source.Entries)-1]
	target := paths.TargetParent.Entries[len(paths.TargetParent.Entries)-1]
	if source.ID == 0 || source.UserID == 0 || source.ParentID == nil || *source.ParentID == 0 || *source.ParentID == source.ID ||
		target.ID == 0 || target.UserID != source.UserID || target.EntryType != "DIR" ||
		paths.Source.Entries[0].ID == 0 || paths.Source.Entries[0].ID != paths.TargetParent.Entries[0].ID ||
		paths.Source.Entries[0].UserID != source.UserID || paths.TargetParent.Entries[0].UserID != source.UserID {
		return domain.ErrInvalidFileEntryData
	}

	// 2. 检查子树统计来源、结构和文件叶子规则。
	if err := validateEntrySubtreeStats(source, stats); err != nil {
		return err
	}

	// 3. 检查包含源自身的子树数量上限。
	if stats.NodeCount > domain.MaxFileEntrySubtreeNodes {
		return domain.ErrFileEntrySubtreeTooLarge
	}

	// 4. 计算移动后最大深度，检查层级上限。
	// 路径包含根目录，所以父目录深度为路径长度减一，源在新位置再加一级。
	targetParentDepth := uint64(len(paths.TargetParent.Entries) - 1)
	movedMaxDepth := targetParentDepth + 1 + stats.MaxRelativeDepth
	if movedMaxDepth > domain.MaxFileEntryDepth {
		return domain.ErrFileEntryDepthExceeded
	}
	return nil
}
