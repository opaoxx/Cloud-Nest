package repository

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateSyncSnapshotSequence 在 WithUserTransaction 的账号锁内复核快照能否衔接增量日志。
func (store *Store) ValidateSyncSnapshotSequence(ctx context.Context, highWaterSequence uint64) error {
	// 1. 由已认证账号事务读取日志区间。
	bounds, err := store.ReadCurrentSyncChangeSequenceBounds(ctx)
	if err != nil {
		return err
	}

	// 2. 根据日志区间判定截止序号无效、过期或可用。
	if highWaterSequence > bounds.HighWaterSequence {
		return domain.ErrInvalidSyncSnapshotSequence
	}
	if highWaterSequence < bounds.MinimumPosition {
		return domain.ErrSyncSnapshotExpired
	}
	return nil
}
