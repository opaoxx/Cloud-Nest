package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PruneSyncChangeLogBatch 在所属账号事务内裁剪一批超过保留期且已确认的连续日志。
// 调用方是可信后台流程。返回范围仅代表已确认提交的删除结果。
func (store *Store) PruneSyncChangeLogBatch(
	ctx context.Context, userID uint64, cutoff time.Time, limit int,
) (domain.SyncChangePruneRange, error) {
	// 1. 锁定所属账号，在事务内筛出本批连续日志。
	var deleted domain.SyncChangePruneRange
	if store == nil || store.database == nil {
		return deleted, fmt.Errorf("裁剪同步日志需要数据入口")
	}
	const allowDisabled = true
	err := store.withLockedUserTransaction(ctx, userID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		prefix, err := txStore.selectSyncChangePrunePrefix(txContext, cutoff, limit)
		if err != nil || prefix.Count == 0 {
			return err
		}

		// 2. 按账号、首尾序号和保留截止点删除，核对实际影响行数。
		const query = "DELETE FROM change_events WHERE user_id = ? AND `sequence` BETWEEN ? AND ? AND created_at < ?"
		result := txStore.database.WithContext(txContext).Exec(
			query, txStore.loginUserID, prefix.FirstSequence, prefix.LastSequence, cutoff.UTC(),
		)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("删除同步日志裁剪范围失败: %w", result.Error)
		}
		if result.RowsAffected != int64(prefix.Count) {
			return fmt.Errorf("%w: 预期 %d 条，实际 %d 条", domain.ErrSyncChangePruneCountMismatch, prefix.Count, result.RowsAffected)
		}
		deleted = prefix
		return nil
	})

	// 3. 提交后返回已删除的范围，失败时返回空结果和错误。
	if err != nil {
		return domain.SyncChangePruneRange{}, err
	}
	return deleted, nil
}
