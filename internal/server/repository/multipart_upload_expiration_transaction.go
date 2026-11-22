package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// WithMultipartUploadExpirationTransaction 锁定并复核一条分片到期候选。
// 回调只使用传入的上下文及 Store 执行数据库操作。访问存储须在事务外进行。
func (store *Store) WithMultipartUploadExpirationTransaction(
	ctx context.Context, candidate domain.UploadExpirationCandidate,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
) error {
	// 1. 核对候选模式，在所属账号事务中锁定任务。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核分片到期候选需要数据入口、上下文和事务回调")
	}
	if candidate.UploadMode != "MULTIPART" || !validUploadExpirationCandidate(candidate) {
		return domain.ErrInvalidUploadTaskData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, candidate.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.readUploadExpirationCandidateTask(txContext, candidate)
		if err != nil {
			return err
		}

		// 2. 复查分片计划、原会话和预占，再锁定仍未发布的暂存对象。
		if !validMultipartUploadCleanupDeclaration(current) {
			return domain.ErrInvalidUploadTaskData
		}
		object, err := lockedStore.FindUserStoredObject(txContext, current.UserID, current.StagedObjectID)
		if err != nil {
			return err
		}
		if !validMultipartUploadCleanupObject(current, object) {
			return domain.ErrInvalidStoredObjectData
		}

		// 3. 拿齐锁后复查 MySQL 时间，再交付原任务与对象的锁内记录。
		if _, err := txStore.checkUploadExpirationTime(txContext, current); err != nil {
			return err
		}
		return action(txContext, txStore, current, object)
	})
}
