package repository

import (
	"context"
	"database/sql"
	"errors"
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

	// 2. 用同一查询读取锁内的当前序号及按账号索引找到的首条保留事件。
	var currentSequence uint64
	var firstRetained sql.Null[uint64]
	err := store.database.WithContext(ctx).Raw(
		"SELECT account.change_seq, (SELECT retained.`sequence` FROM change_events AS retained "+
			"WHERE retained.user_id=account.id ORDER BY retained.`sequence` LIMIT 1) FROM users AS account WHERE account.id=?",
		store.loginUserID,
	).Row().Scan(&currentSequence, &firstRetained)
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrAccountNotFound
	}
	if err != nil {
		return fmt.Errorf("读取账号变更日志边界失败: %w", err)
	}

	// 3. 日志为空时只接受当前序号，有日志时从首条事件的前一位开始。
	minimum := currentSequence
	if firstRetained.Valid {
		if firstRetained.V == 0 || firstRetained.V > currentSequence {
			return domain.ErrInvalidSyncSnapshotSequence
		}
		minimum = firstRetained.V - 1
	}
	if highWaterSequence > currentSequence {
		return domain.ErrInvalidSyncSnapshotSequence
	}
	if highWaterSequence < minimum {
		return domain.ErrSyncSnapshotExpired
	}
	return nil
}
