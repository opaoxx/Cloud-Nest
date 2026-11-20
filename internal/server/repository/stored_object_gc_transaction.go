package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// WithStoredObjectGCTransaction 锁定并复核回收候选，通过后在同一事务内执行回调。
// 回调使用传入的事务 Store 与对象记录，只执行数据库操作。
// 已失效和仍有引用分别返回领域错误，锁随事务结束释放。
func (store *Store) WithStoredObjectGCTransaction(
	ctx context.Context,
	candidate domain.StoredObjectGCCandidate,
	action func(context.Context, *Store, *domain.StoredObject) error,
) error {
	// 1. 校验候选，开启所属用户事务。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核对象回收候选需要数据入口、上下文和事务回调")
	}
	if !candidate.IsValid() {
		return domain.ErrInvalidStoredObjectData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, candidate.OwnerUserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 按归属锁定对象，用 MySQL 当前时间复核是否到期。
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).
			Table("stored_objects FORCE INDEX (uk_objects_owner_id)").
			Where("gc_after <= UTC_TIMESTAMP(6)").
			Clauses(clause.Locking{Strength: "UPDATE"})
		object, err := lockedStore.FindUserStoredObject(txContext, candidate.OwnerUserID, candidate.ObjectID)
		if errors.Is(err, domain.ErrStoredObjectNotFound) {
			return domain.ErrStoredObjectGCCandidateStale
		}
		if err != nil {
			return err
		}

		// 3. 比对状态和计时，复查文件与上传任务引用。
		if object.Status != candidate.Status || object.UnreferencedAt == nil || object.GCAfter == nil ||
			!object.UnreferencedAt.Equal(candidate.UnreferencedAt) || !object.GCAfter.Equal(candidate.GCAfter) {
			return domain.ErrStoredObjectGCCandidateStale
		}
		if err := txStore.checkStoredObjectUnreferenced(txContext, object); err != nil {
			return err
		}

		// 4. 在锁仍持有时，把复核结果交给回调。
		return action(txContext, txStore, object)
	})
}
