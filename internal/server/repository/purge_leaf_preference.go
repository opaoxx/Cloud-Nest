package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// clearPurgeLeafPreference 是永久清理事务中的目录项偏好清理步骤。
// leaf 来自当前事务的 LockPurgeLeaf，删除与目录项完成写入共用外层事务。
func (store *Store) clearPurgeLeafPreference(ctx context.Context, leaf *domain.LockedPurgeLeaf) error {
	// 1. 核对根项事务和锁定叶子的归属。
	if err := store.requireLockedPurgeLeaf(ctx, leaf); err != nil {
		return err
	}

	// 2. 按用户 ID 和目录项 ID 删除偏好记录。
	result := store.database.WithContext(ctx).Exec(
		"DELETE FROM user_entry_preferences WHERE user_id = ? AND entry_id = ?", leaf.UserID, leaf.EntryID)

	// 3. 核对影响行数，交由外层事务提交或回滚。
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("清除永久清理叶子的偏好失败: %w", result.Error)
	}
	// 复合主键最多定位一行，用户未设置偏好或已经删除时返回零行。
	if result.RowsAffected != 0 && result.RowsAffected != 1 {
		return domain.ErrInvalidFileEntryData
	}
	return nil
}
