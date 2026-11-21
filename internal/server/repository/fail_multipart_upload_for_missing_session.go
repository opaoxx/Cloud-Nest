package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// FailMultipartUploadForMissingSession 提交会话丢失终态，返还预占并登记对象延迟回收。
// 只有调用方确认原合并已结束，并在之后观察到会话和对象均缺失，才可传入 true。
func (store *Store) FailMultipartUploadForMissingSession(
	ctx context.Context, provider, bucket string, observation *domain.UploadMergeMissingSessionObservation, mergeFinishedBeforeObservation bool,
) (*domain.UploadTask, error) {
	// 1. 校验缺失观察，加锁复核账号、任务租约和暂存对象。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("终结会话丢失的上传需要数据入口和上下文")
	}
	if observation == nil || !validMultipartMergeClaim(&observation.Task) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if !mergeFinishedBeforeObservation {
		return nil, domain.ErrUploadTaskNotReady
	}
	claimed := &observation.Task
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: observation.ObjectKey}
	if !location.IsValid() || !validMultipartMergeObjectText(location.ObjectKey) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	var failed *domain.UploadTask
	err := store.withActiveUserTransaction(ctx, claimed.UserID, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.lockMultipartMergeTask(txContext, claimed)
		if err != nil {
			return err
		}
		if !current.LeaseUntil.Equal(*claimed.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		account, err := txStore.lockUserUploadQuota(txContext, current.UserID)
		if err != nil {
			return err
		}
		if current.ReservedBytes > account.ReservedBytes {
			return domain.ErrInvalidAccountData
		}
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		staged, err := lockedStore.FindUserStoredObject(txContext, current.UserID, current.StagedObjectID)
		if err != nil {
			return err
		}
		if !matchesUnwrittenMultipartObject(staged, current.FileSize, location) {
			return domain.ErrInvalidStoredObjectData
		}

		// 2. 写入失败终态，同时返还该任务的预占。
		message := domain.ErrUploadSessionLost.Error()
		remainingReserved := account.ReservedBytes - current.ReservedBytes
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE users AS quota_user JOIN upload_tasks AS task ON task.user_id = quota_user.id
			SET quota_user.reserved_storage = ?, quota_user.updated_at = UTC_TIMESTAMP(6),
			    task.status = 'FAILED', task.completed_at = UTC_TIMESTAMP(6), task.updated_at = UTC_TIMESTAMP(6),
			    task.reserved_bytes = 0, task.lease_owner = NULL, task.lease_until = NULL, task.next_attempt_at = NULL,
			    task.last_error_code = ?, task.last_error_message = ?
			WHERE quota_user.id = ? AND quota_user.status = 'ACTIVE'
			  AND quota_user.storage_quota = ? AND quota_user.used_storage = ? AND quota_user.reserved_storage = ?
			  AND task.id = ? AND task.staged_object_id = ? AND task.status = 'COMPLETING' AND task.upload_mode = 'MULTIPART'
			  AND CAST(task.storage_upload_id AS BINARY) = ? AND CAST(task.lease_owner AS BINARY) = ?
			  AND task.lease_version = ? AND task.lease_until = ? AND task.lease_until > UTC_TIMESTAMP(6)
			  AND task.expires_at > UTC_TIMESTAMP(6) AND task.reserved_bytes = task.file_size
			  AND task.completed_at IS NULL AND task.published_object_id IS NULL
			  AND task.result_entry_id IS NULL AND task.resolved_name IS NULL`,
			remainingReserved, domain.UploadSessionLostCode, message,
			current.UserID, account.QuotaBytes, account.UsedBytes, account.ReservedBytes,
			current.ID, staged.ID, *current.StorageUploadID, *current.LeaseOwner, current.LeaseVersion, *current.LeaseUntil)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("写入会话丢失终态失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 2 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读核对，并为无引用的暂存对象登记延迟回收。
		updated, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		if updated.Status != domain.UploadStatusFailed || updated.ReservedBytes != 0 || updated.UploadMode != current.UploadMode ||
			updated.StagedObjectID != staged.ID || !sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) ||
			updated.LeaseVersion != current.LeaseVersion || updated.LeaseOwner != nil || updated.LeaseUntil != nil ||
			updated.NextAttemptAt != nil || updated.AttemptCount != current.AttemptCount ||
			updated.PublishedObjectID != nil || updated.ResultEntryID != nil || updated.ResolvedName != nil ||
			updated.CompletedAt == nil || !updated.CompletedAt.Equal(updated.UpdatedAt) || updated.UpdatedAt.Before(current.UpdatedAt) ||
			!updated.CompletedAt.Before(current.ExpiresAt) || !updated.CompletedAt.Before(*current.LeaseUntil) ||
			updated.LastErrorCode == nil || *updated.LastErrorCode != domain.UploadSessionLostCode ||
			updated.LastErrorMessage == nil || *updated.LastErrorMessage != message || !sameMultipartUploadPlan(current, updated) {
			return domain.ErrInvalidUploadTaskData
		}
		updatedAccount, err := txStore.lockUserUploadQuota(txContext, current.UserID)
		if err != nil {
			return err
		}
		if updatedAccount.UsedBytes != account.UsedBytes || updatedAccount.QuotaBytes != account.QuotaBytes ||
			updatedAccount.ReservedBytes != remainingReserved {
			return domain.ErrInvalidAccountData
		}
		if _, err := lockedStore.markUnreferencedUploadObjectOrphaned(txContext, staged); err != nil {
			return err
		}
		failed = updated
		return nil
	})
	if err != nil {
		return nil, markUploadTransactionRetryError(ctx, err, domain.ErrUploadMergeWriteRetryable)
	}
	return failed, nil
}
