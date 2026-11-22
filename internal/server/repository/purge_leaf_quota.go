package repository

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// releasePurgeLeafQuota 是永久清理事务中的逻辑配额扣减步骤。
// leaf 来自当前事务的 LockPurgeLeaf。调用方把扣减与同一叶子的完成写入一起提交。
func (store *Store) releasePurgeLeafQuota(ctx context.Context, leaf *domain.LockedPurgeLeaf) error {
	// 1. 核对根项事务、归属和锁定结果。
	if err := store.requireLockedPurgeLeaf(ctx, leaf); err != nil {
		return err
	}
	// 2. 释放当前引用的计费字节，目录和零字节文件无需改动账目。
	return store.releaseUsedStorage(ctx, leaf.UserID, leaf.FileSize)
}
