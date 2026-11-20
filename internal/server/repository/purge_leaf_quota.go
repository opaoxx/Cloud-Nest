package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// releasePurgeLeafQuota 是永久清理事务中的逻辑配额扣减步骤。
// leaf 来自当前事务的 LockPurgeLeaf。调用方把扣减与同一叶子的完成写入一起提交。
func (store *Store) releasePurgeLeafQuota(ctx context.Context, leaf *domain.LockedPurgeLeaf) error {
	// 1. 核对根项事务、归属和锁定结果。
	if err := store.requireLockedPurgeLeaf(ctx, leaf); err != nil {
		return err
	}
	// 目录和零字节文件都无需改动账户账目。
	if leaf.FileSize == 0 {
		return ctx.Err()
	}

	// 2. 读取当前配额，检查已用空间是否足够扣减。
	const allowDisabled = true
	account, err := store.lockUploadQuota(ctx, leaf.UserID, allowDisabled)
	if err != nil {
		return err
	}
	if account.UsedBytes < leaf.FileSize {
		return domain.ErrInvalidAccountData
	}
	remainingUsed := account.UsedBytes - leaf.FileSize

	// 3. 按原账目条件更新，交由外层事务提交或回滚。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE users SET used_storage = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE id = ? AND status = ? AND storage_quota = ? AND used_storage = ? AND reserved_storage = ?`,
		remainingUsed, leaf.UserID, account.Status, account.QuotaBytes, account.UsedBytes, account.ReservedBytes)
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("扣减永久清理叶子的逻辑配额失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidAccountData
	}
	return nil
}
