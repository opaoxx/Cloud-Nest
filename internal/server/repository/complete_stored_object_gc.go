package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// CompleteStoredObjectGC 将已确认删除的对象写为 DELETED，提交后返回记录。
// 调用方在存储删除成功或确认目标缺失后，使用事务外 Store 调用。
func (store *Store) CompleteStoredObjectGC(ctx context.Context, deleted *domain.StoredObject) (*domain.StoredObject, error) {
	// 1. 校验删除记录，开启所属用户事务。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("写回存储对象删除结果需要数据入口和上下文")
	}
	if deleted == nil || deleted.ID == 0 || deleted.OwnerUserID == 0 || deleted.Status != domain.StoredObjectStatusDeleting {
		return nil, domain.ErrInvalidStoredObjectData
	}
	location := domain.CreateStagedObjectInput{Provider: deleted.Provider, Bucket: deleted.Bucket, ObjectKey: deleted.ObjectKey}
	if !location.IsValid() {
		return nil, domain.ErrInvalidStoredObjectData
	}

	var completed *domain.StoredObject
	const allowDisabled = true
	err := store.withLockedUserTransaction(ctx, deleted.OwnerUserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 加锁读取并核对对象位置与版本。
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).
			Table("stored_objects FORCE INDEX (uk_objects_owner_id)").
			Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.FindUserStoredObject(txContext, deleted.OwnerUserID, deleted.ID)
		if err != nil {
			return err
		}
		if !sameStoredObjectLocation(current, deleted) {
			return domain.ErrStoredObjectGCCandidateStale
		}

		// 3. 复用已完成结果，或有条件写入 DELETED。
		switch current.Status {
		case domain.StoredObjectStatusDeleted:
			completed = current
			return nil
		case domain.StoredObjectStatusDeleting:
		default:
			return domain.ErrStoredObjectGCCandidateStale
		}
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE stored_objects
			SET status = 'DELETED', last_error_code = NULL, updated_at = UTC_TIMESTAMP(6)
			WHERE owner_user_id = ? AND id = ? AND status = 'DELETING'
			  AND provider = ? AND bucket = ? AND object_key = ? AND storage_version_id <=> ?`,
			current.OwnerUserID, current.ID, current.Provider, current.Bucket, current.ObjectKey, current.StorageVersionID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("写回存储对象删除结果失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrStoredObjectGCCandidateStale
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidStoredObjectData
		}

		// 4. 读回复核，事务提交后返回结果。
		updated, err := lockedStore.FindUserStoredObject(txContext, current.OwnerUserID, current.ID)
		if errors.Is(err, domain.ErrStoredObjectNotFound) {
			return domain.ErrInvalidStoredObjectData
		}
		if err != nil {
			return err
		}
		if updated.Status != domain.StoredObjectStatusDeleted || updated.LastErrorCode != nil || !sameStoredObjectLocation(updated, current) {
			return domain.ErrInvalidStoredObjectData
		}
		completed = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return completed, nil
}
