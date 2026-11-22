package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// CompleteExpiredFileVersion 在同一账号事务内结算一条到期历史，并为失去最后引用的对象开始回收计时。
func (store *Store) CompleteExpiredFileVersion(ctx context.Context, candidate domain.FileVersion) error {
	// 1. 在所属账号事务内加锁复核候选。
	return store.WithExpiredFileVersionTransaction(ctx, candidate,
		func(txContext context.Context, txStore *Store, current domain.FileVersion, object *domain.StoredObject) error {
			// 2. 删除到期历史并扣减对应的已用空间。
			if err := txStore.settleLockedExpiredFileVersion(txContext, current); err != nil {
				return err
			}

			// 3. 有其他有效引用时结算仍成功。其他计时错误交给外层一起回滚。
			if _, err := txStore.StartReadyObjectGCTimer(txContext, object.ID); err != nil && !errors.Is(err, domain.ErrStoredObjectInUse) {
				return err
			}
			return nil
		})
}

// settleLockedExpiredFileVersion 在 WithExpiredFileVersionTransaction 的回调内结算已复核的历史行。
// 调用方须将错误交回外层事务，历史删除和计费扣减在事务提交成功后一起生效。
func (store *Store) settleLockedExpiredFileVersion(ctx context.Context, version domain.FileVersion) error {
	// 1. 校验事务及锁内读回的历史归属。
	if store == nil || store.database == nil || ctx == nil || store.loginUserID == 0 {
		return fmt.Errorf("结算到期历史版本需要所属账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("结算到期历史版本必须使用事务 Store")
	}
	if !version.IsValid() || version.UserID != store.loginUserID {
		return domain.ErrInvalidFileVersionData
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. 按复核快照删除确切历史行，零行表示该候选不能再结算。
	result := store.database.WithContext(ctx).Exec(`DELETE FROM file_versions
		WHERE user_id = ? AND id = ? AND object_id = ? AND entry_id = ? AND revision = ?
		  AND file_size = ? AND created_at = ? AND retained_until = ? AND retained_until <= UTC_TIMESTAMP(6)`,
		version.UserID, version.ID, version.ObjectID, version.EntryID, version.Revision,
		version.FileSize, version.CreatedAt.UTC(), version.RetainedUntil.UTC())
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("删除到期历史版本失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.ErrExpiredFileVersionCandidateStale
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidFileVersionData
	}

	// 3. 扣减已用空间，交由外层事务统一提交或回滚。
	return store.releaseUsedStorage(ctx, version.UserID, version.FileSize)
}
