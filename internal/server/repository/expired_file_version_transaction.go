package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WithExpiredFileVersionTransaction 在所属账号事务内加锁复核单条历史候选。
// 回调只能使用传入的事务上下文和 Store，锁在回调结束后随事务释放。
func (store *Store) WithExpiredFileVersionTransaction(
	ctx context.Context,
	candidate domain.FileVersion,
	action func(context.Context, *Store, domain.FileVersion, *domain.StoredObject) error,
) error {
	// 1. 校验候选，锁定所属账号。后台结算也覆盖已禁用账号的历史。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核到期历史版本需要数据入口、上下文和事务回调")
	}
	if !candidate.IsValid() {
		return domain.ErrInvalidFileVersionData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, candidate.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 按归属先锁对象，再锁历史行，并以 MySQL 当前 UTC 时间复核到期条件。
		lockedObjectStore := *txStore
		lockedObjectStore.database = txStore.database.WithContext(txContext).
			Table("stored_objects FORCE INDEX (uk_objects_owner_id)").Clauses(clause.Locking{Strength: "UPDATE"})
		object, err := lockedObjectStore.FindUserStoredObject(txContext, candidate.UserID, candidate.ObjectID)
		if errors.Is(err, domain.ErrStoredObjectNotFound) {
			return domain.ErrExpiredFileVersionCandidateStale
		}
		if err != nil {
			return err
		}

		var current domain.FileVersion
		queryError := txStore.database.WithContext(txContext).
			Table("file_versions FORCE INDEX (idx_file_versions_owner_object)").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND object_id = ? AND id = ? AND retained_until <= UTC_TIMESTAMP(6)",
				candidate.UserID, candidate.ObjectID, candidate.ID).Take(&current).Error
		if errors.Is(queryError, gorm.ErrRecordNotFound) {
			return domain.ErrExpiredFileVersionCandidateStale
		}
		if queryError != nil {
			return fmt.Errorf("锁定到期历史版本失败: %w", queryError)
		}

		// 3. 核对扫描快照和对象状态，通过后在锁内执行回调。
		current.CreatedAt = current.CreatedAt.UTC()
		current.RetainedUntil = current.RetainedUntil.UTC()
		if !current.IsValid() {
			return domain.ErrInvalidFileVersionData
		}
		if current.EntryID != candidate.EntryID || current.Revision != candidate.Revision || current.FileSize != candidate.FileSize ||
			!current.CreatedAt.Equal(candidate.CreatedAt) || !current.RetainedUntil.Equal(candidate.RetainedUntil) {
			return domain.ErrExpiredFileVersionCandidateStale
		}
		if object.Status != domain.StoredObjectStatusReady || object.VerifiedSHA256 == nil || object.VerifiedAt == nil ||
			object.FileSize != current.FileSize || object.UnreferencedAt != nil || object.GCAfter != nil {
			return domain.ErrInvalidStoredObjectData
		}
		return action(txContext, txStore, current, object)
	})
}
