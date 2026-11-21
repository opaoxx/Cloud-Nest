package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CompleteMissingMultipartUploadCancellation 提交对象明确缺失的取消终态。
// 同一租约的重复交付复用已提交结果。临时数据库错误附带取消重试标记。
func (store *Store) CompleteMissingMultipartUploadCancellation(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCancellationObservation,
) (*domain.UploadTask, error) {
	// 1. 复核缺失观察，识别同一次取消的已提交结果。
	if observation == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if observation.Object != nil {
		return nil, domain.ErrUploadTaskNotReady
	}
	var completed *domain.UploadTask
	err := store.withMultipartUploadCancellationObservationTransaction(ctx, provider, bucket, observation,
		func(txContext context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
			// 2. 按有效租约写入 CANCELLED，返还该任务的预占。
			finished, err := txStore.writeMultipartUploadCancelled(txContext, current)
			if err != nil {
				return err
			}
			// 3. 复用无引用检查，为暂存对象登记延迟回收，统一提交。
			orphaned, err := txStore.markUnreferencedUploadObjectOrphaned(txContext, staged)
			if err != nil {
				return err
			}
			if !hasMissingMultipartCancellationObject(orphaned, finished) {
				return domain.ErrInvalidStoredObjectData
			}
			completed = finished
			return nil
		},
		func(current *domain.UploadTask, staged *domain.StoredObject) error {
			if err := validateCompletedMultipartCancellationTask(current, &observation.Task); err != nil {
				return err
			}
			if !hasMissingMultipartCancellationObject(staged, current) {
				return domain.ErrInvalidStoredObjectData
			}
			completed = current
			return nil
		})
	if err != nil {
		return nil, markUploadTransactionRetryError(ctx, err, domain.ErrUploadCancellationRetryable)
	}
	return completed, nil
}

// writeMultipartUploadCancelled 更新已经加锁复核的任务和账号账目。
func (store *Store) writeMultipartUploadCancelled(ctx context.Context, current *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 核对账号预占，计算返还后的余额。
	const allowDisabled = true
	account, err := store.lockUploadQuota(ctx, current.UserID, allowDisabled)
	if err != nil {
		return nil, err
	}
	if current.ReservedBytes > account.ReservedBytes {
		return nil, domain.ErrInvalidAccountData
	}
	remainingReserved := account.ReservedBytes - current.ReservedBytes

	// 2. 条件更新两行，以实际写入时的 UTC 时间再次检查租约。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE users AS quota_user JOIN upload_tasks AS task ON task.user_id = quota_user.id
		SET quota_user.reserved_storage = ?, quota_user.updated_at = UTC_TIMESTAMP(6),
		    task.status = 'CANCELLED', task.reserved_bytes = 0,
		    task.completed_at = UTC_TIMESTAMP(6), task.updated_at = UTC_TIMESTAMP(6),
		    task.lease_owner = NULL, task.lease_until = NULL, task.next_attempt_at = NULL,
		    task.last_error_code = NULL, task.last_error_message = NULL
		WHERE quota_user.id = ? AND quota_user.status = ?
		  AND quota_user.storage_quota = ? AND quota_user.used_storage = ? AND quota_user.reserved_storage = ?
		  AND task.id = ? AND task.staged_object_id = ? AND task.status = 'CANCELLING' AND task.upload_mode = 'MULTIPART'
		  AND CAST(task.storage_upload_id AS BINARY) <=> ? AND CAST(task.lease_owner AS BINARY) = ?
		  AND task.lease_version = ? AND task.lease_until = ? AND task.lease_until > UTC_TIMESTAMP(6)
		  AND (task.next_attempt_at IS NULL OR task.next_attempt_at <= UTC_TIMESTAMP(6))
		  AND task.reserved_bytes = task.file_size AND task.reserved_bytes = ?
		  AND task.published_object_id IS NULL AND task.result_entry_id IS NULL
		  AND task.resolved_name IS NULL AND task.completed_at IS NULL`,
		remainingReserved, current.UserID, account.Status, account.QuotaBytes, account.UsedBytes, account.ReservedBytes,
		current.ID, current.StagedObjectID, current.StorageUploadID, *current.LeaseOwner,
		current.LeaseVersion, *current.LeaseUntil, current.ReservedBytes)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("写入分片上传取消终态失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 2 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 回读任务和账目，保持原分片计划以及账号已用空间。
	finished, err := store.FindUserUploadTask(ctx, current.UserID, current.ID)
	if err != nil {
		return nil, err
	}
	if err := validateCompletedMultipartCancellationTask(finished, current); err != nil {
		return nil, err
	}
	updatedAccount, err := store.lockUploadQuota(ctx, current.UserID, allowDisabled)
	if err != nil {
		return nil, err
	}
	if updatedAccount.Status != account.Status || updatedAccount.UsedBytes != account.UsedBytes ||
		updatedAccount.QuotaBytes != account.QuotaBytes || updatedAccount.ReservedBytes != remainingReserved {
		return nil, domain.ErrInvalidAccountData
	}
	return finished, nil
}

func validateCompletedMultipartCancellationTask(finished, claimed *domain.UploadTask) error {
	if finished.UploadMode != "MULTIPART" || finished.StagedObjectID != claimed.StagedObjectID ||
		finished.LeaseVersion != claimed.LeaseVersion || !sameUploadStorageText(finished.StorageUploadID, claimed.StorageUploadID) {
		return domain.ErrUploadTaskBusy
	}
	if !hasCompletedUploadCleanup(finished, domain.UploadStatusCancelled) || !sameMultipartUploadPlan(claimed, finished) ||
		finished.AttemptCount != claimed.AttemptCount || finished.PublishedObjectID != nil || finished.ResultEntryID != nil ||
		finished.ResolvedName != nil || finished.CompletedAt.Before(claimed.UpdatedAt) || !finished.CompletedAt.Before(*claimed.LeaseUntil) {
		return domain.ErrInvalidUploadTaskData
	}
	return nil
}

func hasMissingMultipartCancellationObject(staged *domain.StoredObject, finished *domain.UploadTask) bool {
	// 复用终态时兼容原记录已经进入回收流程。
	return (staged.Status == domain.StoredObjectStatusOrphaned || staged.Status == domain.StoredObjectStatusDeleting ||
		staged.Status == domain.StoredObjectStatusDeleted) &&
		staged.FileSize == finished.FileSize && staged.StorageETag == nil && staged.StorageVersionID == nil &&
		staged.VerifiedSHA256 == nil && staged.VerifiedAt == nil && staged.UnreferencedAt != nil && staged.GCAfter != nil &&
		!staged.UnreferencedAt.Before(*finished.CompletedAt) && !staged.GCAfter.Before(staged.UnreferencedAt.Add(7*24*time.Hour))
}
