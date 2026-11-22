package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// releaseUsedStorage 为所属账号事务内已核对的逻辑引用释放已用空间。
func (store *Store) releaseUsedStorage(ctx context.Context, userID, sizeBytes uint64) error {
	// 1. 零字节无需写账目。否则在锁内检查账目及扣减下界。
	if sizeBytes == 0 {
		return ctx.Err()
	}
	const allowDisabled = true
	account, err := store.lockUploadQuota(ctx, userID, allowDisabled)
	if err != nil {
		return err
	}
	if account.UsedBytes < sizeBytes {
		return domain.ErrInvalidAccountData
	}

	// 2. 按账户原账目条件更新，只扣减已用空间。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE users SET used_storage = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE id = ? AND status = ? AND storage_quota = ? AND used_storage = ? AND reserved_storage = ?`,
		account.UsedBytes-sizeBytes, userID, account.Status, account.QuotaBytes, account.UsedBytes, account.ReservedBytes)

	// 3. 检查本次更新，交由外层事务确认。
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("扣减逻辑已用空间失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidAccountData
	}
	return nil
}
