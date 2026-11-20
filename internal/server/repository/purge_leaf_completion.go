package repository

import (
	"context"
	"fmt"
	"math"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// markPurgeLeafCompleted 是单项清理事务中的状态写入步骤。
// leaf 须来自当前根项事务的 LockPurgeLeaf，配额和偏好由外层在同一事务结算。
func (store *Store) markPurgeLeafCompleted(ctx context.Context, leaf *domain.LockedPurgeLeaf) error {
	// 1. 核对根项事务和锁定结果。
	if err := store.requireLockedPurgeLeaf(ctx, leaf); err != nil {
		return err
	}

	// 2. 按原状态、版本和对象引用执行条件更新。
	nextRevision := leaf.Revision
	// 已受理的终态推进允许沿用最高版本，由状态条件防止再次完成。
	if nextRevision < math.MaxUint64 {
		nextRevision++
	}
	result := store.database.WithContext(ctx).Exec(`
		UPDATE file_entries
		SET status = 'PURGED', object_id = NULL, purged_at = UTC_TIMESTAMP(6),
		    revision = ?, modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
		WHERE user_id = ? AND id = ? AND parent_id = ? AND entry_type = ?
		  AND status = ? AND revision = ? AND file_size = ? AND object_id <=> ?
		  AND purged_at IS NULL AND (purge_started_at IS NULL OR purge_started_at <= UTC_TIMESTAMP(6))`,
		nextRevision, leaf.UserID, leaf.EntryID, leaf.ParentID, leaf.EntryType,
		leaf.Status, leaf.Revision, leaf.FileSize, leaf.ObjectID)

	// 3. 检查影响行数，交由外层事务确认结果。
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("写入叶子清理完成状态失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.ErrPurgeLeafCandidateStale
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidFileEntryData
	}
	return nil
}
