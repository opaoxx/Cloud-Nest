package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// WithMultipartUploadCancellationTransaction 锁定并复核一条已受理的分片取消候选。
// 回调在锁释放前执行，只使用传入的事务上下文和 Store 进行数据库操作。
func (store *Store) WithMultipartUploadCancellationTransaction(
	ctx context.Context,
	candidate domain.UploadCancellationCandidate,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
) error {
	// 1. 校验候选，开启所属账号事务；已禁用账号的既有取消仍可收尾。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核分片取消候选需要数据入口、上下文和事务回调")
	}
	if candidate.TaskID == 0 || candidate.UserID == 0 || candidate.StagedObjectID == 0 {
		return domain.ErrInvalidUploadTaskData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, candidate.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 加锁复核状态、候选快照及当前租约和重试时间。
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		task, err := lockedStore.FindUserUploadTask(txContext, candidate.UserID, candidate.TaskID)
		if err != nil {
			return err
		}
		if task.Status != domain.UploadStatusCancelling || task.UploadMode != "MULTIPART" {
			return domain.ErrUploadTaskNotReady
		}
		if task.StagedObjectID != candidate.StagedObjectID || task.LeaseVersion != candidate.LeaseVersion {
			return domain.ErrUploadTaskBusy
		}
		if !validPendingMultipartUploadCancellationTask(task) {
			return domain.ErrInvalidUploadTaskData
		}
		var now time.Time
		clockError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
		if err := txContext.Err(); err != nil {
			return err
		}
		if clockError != nil {
			return fmt.Errorf("读取分片取消复核时间失败: %w", clockError)
		}
		if (task.LeaseUntil != nil && now.Before(*task.LeaseUntil)) || (task.NextAttemptAt != nil && now.Before(*task.NextAttemptAt)) {
			return domain.ErrUploadTaskBusy
		}

		// 3. 锁定原暂存对象，确认仍处于未发布、未回收状态，再执行回调。
		object, err := lockedStore.FindUserStoredObject(txContext, task.UserID, task.StagedObjectID)
		if err != nil {
			return err
		}
		if !validMultipartUploadCleanupObject(task, object) {
			return domain.ErrInvalidStoredObjectData
		}
		return action(txContext, txStore, task, object)
	})
}

func validPendingMultipartUploadCancellationTask(task *domain.UploadTask) bool {
	return validMultipartUploadCleanupDeclaration(task) &&
		task.PublishedObjectID == nil && task.ResultEntryID == nil && task.ResolvedName == nil && task.CompletedAt == nil
}

func validMultipartUploadCleanupObject(task *domain.UploadTask, object *domain.StoredObject) bool {
	return (object.Status == domain.StoredObjectStatusStaging || object.Status == domain.StoredObjectStatusVerifying) &&
		object.FileSize == task.FileSize && object.VerifiedAt == nil && object.VerifiedSHA256 == nil &&
		object.DedupHash == nil && object.UnreferencedAt == nil && object.GCAfter == nil
}
