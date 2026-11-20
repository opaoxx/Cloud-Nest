package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidatePurgePreviewTarget 验证永久清理预检的目标是当前用户的顶层回收项。
// 调用方使用同一只读快照或已认证用户事务继续读取子树。
func ValidatePurgePreviewTarget(ctx context.Context, actor domain.Actor, entryID uint64, reader FileEntryPathReader) (*domain.FileEntry, error) {
	// 1. 按当前用户读取目标，拒绝真实根目录和非回收状态。
	target, err := readTrashedEntryTarget(ctx, actor, entryID, reader, domain.ErrRootEntryPurge)
	if err != nil {
		return nil, err
	}

	// 2. 沿原父目录一路核对到真实根目录。
	ancestors, err := readTrashedEntryAncestors(ctx, actor, target, reader)
	if err != nil {
		return nil, err
	}

	// 3. 祖先全部为活动目录时，返回可预检的目标记录。
	if ancestors.HasTrashedAncestor {
		return nil, domain.ErrFileEntryNotFound
	}
	return target, nil
}
