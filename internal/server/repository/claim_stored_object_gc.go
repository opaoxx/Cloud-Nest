package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ClaimStoredObjectGC 复核到期候选，提交删除意图后返回 DELETING 对象。
// 使用事务外 Store 调用，原候选在成功领取后失效。
func (store *Store) ClaimStoredObjectGC(ctx context.Context, candidate domain.StoredObjectGCCandidate) (*domain.StoredObject, error) {
	// 1. 按所属用户开启事务，锁定并复核到期对象。
	var claimed *domain.StoredObject
	err := store.WithStoredObjectGCTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, object *domain.StoredObject) error {
		// 2. 匹配原状态和计时，写入 DELETING。
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE stored_objects
			SET status = 'DELETING', updated_at = UTC_TIMESTAMP(6)
			WHERE owner_user_id = ? AND id = ? AND status = ?
			  AND unreferenced_at = ? AND gc_after = ? AND gc_after <= UTC_TIMESTAMP(6)`,
			object.OwnerUserID, object.ID, object.Status, *object.UnreferencedAt, *object.GCAfter)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("写入对象删除意图失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrStoredObjectGCCandidateStale
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidStoredObjectData
		}

		// 3. 读回并核对状态、计时和存储位置。
		updated, err := txStore.FindUserStoredObject(txContext, object.OwnerUserID, object.ID)
		if errors.Is(err, domain.ErrStoredObjectNotFound) {
			return domain.ErrInvalidStoredObjectData
		}
		if err != nil {
			return err
		}
		if updated.Status != domain.StoredObjectStatusDeleting || updated.UnreferencedAt == nil || updated.GCAfter == nil ||
			!updated.UnreferencedAt.Equal(*object.UnreferencedAt) || !updated.GCAfter.Equal(*object.GCAfter) ||
			updated.Provider != object.Provider || updated.Bucket != object.Bucket || updated.ObjectKey != object.ObjectKey ||
			(updated.StorageVersionID == nil) != (object.StorageVersionID == nil) ||
			(updated.StorageVersionID != nil && *updated.StorageVersionID != *object.StorageVersionID) {
			return domain.ErrInvalidStoredObjectData
		}
		claimed = updated
		return nil
	})

	// 4. 确认事务提交后，返回对象记录。
	if err != nil {
		return nil, err
	}
	return claimed, nil
}
