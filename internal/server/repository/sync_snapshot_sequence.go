package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ValidateSyncSnapshotSequence 在 WithUserTransaction 的账号锁内复核快照能否衔接增量日志。
func (store *Store) ValidateSyncSnapshotSequence(ctx context.Context, highWaterSequence uint64) error {
	// 1. 确认调用来自已复查身份的账号事务，归属只取自事务。
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 || store.authenticatedActor == nil {
		return fmt.Errorf("复核快照截止序号需要已认证账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("复核快照截止序号必须使用账号事务 Store")
	}
	if store.authenticatedActor.UserID != store.loginUserID || store.authenticatedActor.DeviceID == 0 ||
		store.authenticatedActor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. 读取锁内的日志区间，沿用快照确认的上界与过期判定。
	bounds, err := store.readSyncChangeSequenceBounds(ctx, store.loginUserID)
	if err != nil {
		return err
	}
	if highWaterSequence > bounds.HighWaterSequence {
		return domain.ErrInvalidSyncSnapshotSequence
	}
	if highWaterSequence < bounds.MinimumPosition {
		return domain.ErrSyncSnapshotExpired
	}
	return nil
}
