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
	return store.completeMultipartUploadCancellation(ctx, provider, bucket, observation)
}

func (store *Store) completeMultipartUploadCancellation(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCancellationObservation,
) (*domain.UploadTask, error) {
	completed, err := store.completeMultipartUploadCleanup(ctx, provider, bucket, observation,
		domain.UploadStatusCancelling, domain.UploadStatusCancelled)
	return completed, markUploadTransactionRetryError(ctx, err, domain.ErrUploadCancellationRetryable)
}

func (store *Store) completeMultipartUploadCleanup(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCleanupObservation,
	pendingStatus, terminalStatus domain.UploadStatus,
) (*domain.UploadTask, error) {
	// 1. 锁内复核对象观察，识别同一租约已提交的结果。
	observed := observation.Object
	var completed *domain.UploadTask
	err := store.withMultipartUploadCleanupObservationTransaction(ctx, provider, bucket, observation, pendingStatus, terminalStatus,
		func(txContext context.Context, txStore *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
			// 2. 按实际写入时间检查任务期限和租约，同时写入终态与返还预占。
			finished, err := txStore.writeMultipartUploadCleanup(txContext, current, terminalStatus)
			if err != nil {
				return err
			}
			// 3. 补齐尚未登记的版本与 ETag，确认无引用后登记七天回收宽限。
			if observed != nil && staged.StorageETag == nil {
				if err := txStore.saveMultipartCleanupObjectLocation(txContext, staged, observed); err != nil {
					return err
				}
			}
			orphaned, err := txStore.markUnreferencedUploadObjectOrphaned(txContext, staged)
			if err != nil {
				return err
			}
			if !hasCompletedMultipartCleanupObject(orphaned, finished, observed) {
				return domain.ErrInvalidStoredObjectData
			}
			completed = finished
			return nil
		},
		func(current *domain.UploadTask, staged *domain.StoredObject) error {
			if err := validateCompletedMultipartCleanupTask(current, &observation.Task, terminalStatus); err != nil {
				return err
			}
			if !hasCompletedMultipartCleanupObject(staged, current, observed) {
				return domain.ErrInvalidStoredObjectData
			}
			completed = current
			return nil
		})
	if err != nil {
		return nil, err
	}
	return completed, nil
}

// writeMultipartUploadCleanup 更新已经加锁复核的任务和账号账目。
func (store *Store) writeMultipartUploadCleanup(ctx context.Context, current *domain.UploadTask, terminalStatus domain.UploadStatus) (*domain.UploadTask, error) {
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

	// 2. 条件更新两行。租约仍有效，且到期收尾已经达到原任务期限。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE users AS quota_user JOIN upload_tasks AS task ON task.user_id = quota_user.id
		SET quota_user.reserved_storage = ?, quota_user.updated_at = UTC_TIMESTAMP(6),
		    task.status = ?, task.reserved_bytes = 0,
		    task.completed_at = UTC_TIMESTAMP(6), task.updated_at = UTC_TIMESTAMP(6),
		    task.lease_owner = NULL, task.lease_until = NULL, task.next_attempt_at = NULL,
		    task.last_error_code = NULL, task.last_error_message = NULL
		WHERE quota_user.id = ? AND quota_user.status = ?
		  AND quota_user.storage_quota = ? AND quota_user.used_storage = ? AND quota_user.reserved_storage = ?
		  AND task.id = ? AND task.staged_object_id = ? AND task.status = ? AND task.upload_mode = 'MULTIPART'
		  AND task.expires_at = ? AND (task.status = 'CANCELLING' OR task.expires_at <= UTC_TIMESTAMP(6))
		  AND CAST(task.storage_upload_id AS BINARY) <=> ? AND CAST(task.lease_owner AS BINARY) = ?
		  AND task.lease_version = ? AND task.lease_until = ? AND task.lease_until > UTC_TIMESTAMP(6)
		  AND (task.next_attempt_at IS NULL OR task.next_attempt_at <= UTC_TIMESTAMP(6))
		  AND task.reserved_bytes = task.file_size AND task.reserved_bytes = ?
		  AND task.published_object_id IS NULL AND task.result_entry_id IS NULL
		  AND task.resolved_name IS NULL AND task.completed_at IS NULL`,
		remainingReserved, terminalStatus, current.UserID, account.Status, account.QuotaBytes, account.UsedBytes, account.ReservedBytes,
		current.ID, current.StagedObjectID, current.Status, current.ExpiresAt, current.StorageUploadID, *current.LeaseOwner,
		current.LeaseVersion, *current.LeaseUntil, current.ReservedBytes)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("写入分片上传终态 %s 失败: %w", terminalStatus, result.Error)
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
	if err := validateCompletedMultipartCleanupTask(finished, current, terminalStatus); err != nil {
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

func validateCompletedMultipartCleanupTask(finished, claimed *domain.UploadTask, terminalStatus domain.UploadStatus) error {
	if finished.UploadMode != "MULTIPART" || finished.StagedObjectID != claimed.StagedObjectID ||
		finished.LeaseVersion != claimed.LeaseVersion || !sameUploadStorageText(finished.StorageUploadID, claimed.StorageUploadID) {
		return domain.ErrUploadTaskBusy
	}
	if !hasCompletedUploadCleanup(finished, terminalStatus) || !sameMultipartUploadPlan(claimed, finished) ||
		finished.AttemptCount != claimed.AttemptCount || finished.PublishedObjectID != nil || finished.ResultEntryID != nil ||
		finished.ResolvedName != nil || finished.CompletedAt.Before(claimed.UpdatedAt) || !finished.CompletedAt.Before(*claimed.LeaseUntil) {
		return domain.ErrInvalidUploadTaskData
	}
	return nil
}

func hasCompletedMultipartCleanupObject(
	staged *domain.StoredObject, finished *domain.UploadTask, observed *domain.UploadObjectResult,
) bool {
	// 复用终态时兼容已进入回收流程的记录，保留原声明大小及未校验事实。
	registered := (staged.Status == domain.StoredObjectStatusOrphaned || staged.Status == domain.StoredObjectStatusDeleting ||
		staged.Status == domain.StoredObjectStatusDeleted) &&
		staged.FileSize == finished.FileSize &&
		staged.VerifiedSHA256 == nil && staged.VerifiedAt == nil && staged.UnreferencedAt != nil && staged.GCAfter != nil &&
		!staged.UnreferencedAt.Before(*finished.CompletedAt) && !staged.GCAfter.Before(staged.UnreferencedAt.Add(7*24*time.Hour))
	if !registered {
		return false
	}
	if observed == nil {
		return staged.StorageETag == nil && staged.StorageVersionID == nil
	}
	return sameUploadStorageText(staged.StorageETag, observed.StorageETag) &&
		sameUploadStorageText(staged.StorageVersionID, observed.StorageVersionID)
}
