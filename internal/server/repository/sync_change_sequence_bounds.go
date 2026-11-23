package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ReadSyncChangeSequenceBounds 在 WithSyncChangePullSnapshot 回调内读取该账号的可续读区间。
func (store *Store) ReadSyncChangeSequenceBounds(ctx context.Context) (domain.SyncChangeSequenceBounds, error) {
	// 1. 账号只取自已核对身份的拉取事务。
	if ctx == nil || store == nil || store.syncChangePullUserID == 0 {
		return domain.SyncChangeSequenceBounds{}, fmt.Errorf("读取增量日志边界需要上下文和已绑定账号的拉取读视图")
	}
	if err := ctx.Err(); err != nil {
		return domain.SyncChangeSequenceBounds{}, err
	}
	return store.readSyncChangeSequenceBounds(ctx, store.syncChangePullUserID)
}

func (store *Store) readSyncChangeSequenceBounds(ctx context.Context, userID uint64) (domain.SyncChangeSequenceBounds, error) {
	// 1. 在同一查询读取当前序号和按账号索引定位的首条保留事件。
	var currentSequence uint64
	var firstRetained sql.Null[uint64]
	err := store.database.WithContext(ctx).Raw(
		"SELECT account.change_seq, (SELECT retained.`sequence` FROM change_events AS retained "+
			"WHERE retained.user_id=account.id ORDER BY retained.`sequence` LIMIT 1) FROM users AS account WHERE account.id=?",
		userID,
	).Row().Scan(&currentSequence, &firstRetained)
	if contextError := ctx.Err(); contextError != nil {
		return domain.SyncChangeSequenceBounds{}, contextError
	}
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SyncChangeSequenceBounds{}, domain.ErrAccountNotFound
	}
	if err != nil {
		return domain.SyncChangeSequenceBounds{}, fmt.Errorf("读取账号变更日志边界失败: %w", err)
	}

	// 2. 日志为空时位置只能是当前序号。有日志时先检查首条序号，再减一得到下界。
	minimum := currentSequence
	if firstRetained.Valid {
		if firstRetained.V == 0 || firstRetained.V > currentSequence {
			return domain.SyncChangeSequenceBounds{}, domain.ErrInvalidSyncSnapshotSequence
		}
		minimum = firstRetained.V - 1
	}
	return domain.SyncChangeSequenceBounds{MinimumPosition: minimum, HighWaterSequence: currentSequence}, nil
}
