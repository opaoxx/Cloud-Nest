package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// CompleteUploadExpiration 终结已进入过期清理的单文件任务，提交后返回终态。
// 同一次过期处理的重复调用复用已提交的 EXPIRED 结果。
// 已确认的临时数据库错误附带 ErrUploadExpirationRetryable 标记。
func (store *Store) CompleteUploadExpiration(ctx context.Context, task *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 完成过期收尾，必要时为数据库错误附加过期重试标记。
	finished, err := store.completeSingleUploadCleanup(ctx, task, domain.UploadStatusExpiring, domain.UploadStatusExpired, "终结过期上传")
	return finished, markUploadTransactionRetryError(ctx, err, domain.ErrUploadExpirationRetryable)
}

// CompleteUploadCancellation 终结已受理取消的单文件任务，提交后返回终态。
// 后台调用方传入已提交的 CANCELLING 任务，重复调用复用同一 CANCELLED 结果。
// 已确认的临时数据库错误附带 ErrUploadCancellationRetryable 标记。
func (store *Store) CompleteUploadCancellation(ctx context.Context, task *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 使用同一套单文件收尾事务推进 CANCELLING → CANCELLED。
	finished, err := store.completeSingleUploadCleanup(ctx, task, domain.UploadStatusCancelling, domain.UploadStatusCancelled, "终结取消上传")
	// 2. 识别临时数据库错误并附加取消重试标记。
	return finished, markUploadTransactionRetryError(ctx, err, domain.ErrUploadCancellationRetryable)
}

// completeSingleUploadCleanup 在同一用户事务收尾单文件取消或过期。
func (store *Store) completeSingleUploadCleanup(
	ctx context.Context, task *domain.UploadTask, pendingStatus, terminalStatus domain.UploadStatus, operation string,
) (*domain.UploadTask, error) {
	// 1. 锁定所属用户和任务，复核本次收尾的身份和阶段。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("%s需要数据入口和上下文", operation)
	}
	if task == nil || task.UserID == 0 || task.ID == 0 || task.StagedObjectID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.UploadMode != "SINGLE" || (task.Status != pendingStatus && task.Status != terminalStatus) {
		return nil, domain.ErrUploadTaskNotReady
	}

	var completed *domain.UploadTask
	const allowDisabled = true
	err := store.withLockedUserTransaction(ctx, task.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.FindUserUploadTask(txContext, task.UserID, task.ID)
		if err != nil {
			return err
		}
		if current.UploadMode != "SINGLE" || (current.Status != pendingStatus && current.Status != terminalStatus) {
			return domain.ErrUploadTaskNotReady
		}
		if current.StagedObjectID != task.StagedObjectID || current.LeaseVersion != task.LeaseVersion ||
			!current.ExpiresAt.Equal(task.ExpiresAt) || current.FileSize != task.FileSize {
			return domain.ErrUploadTaskBusy
		}
		if current.PublishedObjectID != nil || current.ResultEntryID != nil || current.ResolvedName != nil {
			return domain.ErrInvalidUploadTaskData
		}
		if current.Status == terminalStatus {
			if !hasCompletedUploadCleanup(current, terminalStatus) {
				return domain.ErrInvalidUploadTaskData
			}
			completed = current
			return nil
		}
		if task.Status != pendingStatus || current.ReservedBytes != task.ReservedBytes || current.CompletedAt != nil {
			return domain.ErrInvalidUploadTaskData
		}

		// 2. 核对账户预占和暂存对象，写入终态并按任务实际预占返还。
		account, err := txStore.lockUploadQuota(txContext, current.UserID, allowDisabled)
		if err != nil {
			return err
		}
		if current.ReservedBytes > account.ReservedBytes {
			return domain.ErrInvalidAccountData
		}
		staged, err := lockedStore.FindUserStoredObject(txContext, current.UserID, current.StagedObjectID)
		if err != nil {
			return err
		}
		if (staged.Status != domain.StoredObjectStatusStaging && staged.Status != domain.StoredObjectStatusVerifying) ||
			staged.FileSize != current.FileSize || staged.UnreferencedAt != nil || staged.VerifiedAt != nil || staged.VerifiedSHA256 != nil {
			return domain.ErrInvalidStoredObjectData
		}
		// 取消已经受理，可在原上传期限前后收尾。过期收尾须等原期限到达。
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE users AS quota_user JOIN upload_tasks AS upload_task ON upload_task.user_id = quota_user.id "+
				"SET quota_user.reserved_storage = ?, quota_user.updated_at = IF(? = 0, quota_user.updated_at, UTC_TIMESTAMP(6)), "+
				"upload_task.status = ?, upload_task.reserved_bytes = 0, "+
				"upload_task.completed_at = UTC_TIMESTAMP(6), upload_task.updated_at = UTC_TIMESTAMP(6), "+
				"upload_task.lease_owner = NULL, upload_task.lease_until = NULL, upload_task.next_attempt_at = NULL, "+
				"upload_task.last_error_code = NULL, upload_task.last_error_message = NULL "+
				"WHERE quota_user.id = ? AND quota_user.status = ? "+
				"AND quota_user.storage_quota = ? AND quota_user.used_storage = ? AND quota_user.reserved_storage = ? "+
				"AND upload_task.id = ? AND upload_task.staged_object_id = ? AND upload_task.lease_version = ? "+
				"AND upload_task.status = ? AND upload_task.upload_mode = 'SINGLE' "+
				"AND upload_task.file_size = ? AND upload_task.reserved_bytes = ? "+
				"AND upload_task.expires_at = ? "+
				"AND (upload_task.status = 'CANCELLING' OR upload_task.expires_at <= UTC_TIMESTAMP(6)) "+
				"AND (upload_task.lease_until IS NULL OR upload_task.lease_until <= UTC_TIMESTAMP(6)) "+
				"AND (upload_task.next_attempt_at IS NULL OR upload_task.next_attempt_at <= UTC_TIMESTAMP(6)) "+
				"AND upload_task.published_object_id IS NULL AND upload_task.result_entry_id IS NULL "+
				"AND upload_task.resolved_name IS NULL AND upload_task.completed_at IS NULL",
			account.ReservedBytes-current.ReservedBytes, current.ReservedBytes, terminalStatus,
			current.UserID, account.Status, account.QuotaBytes, account.UsedBytes, account.ReservedBytes,
			current.ID, staged.ID, current.LeaseVersion, pendingStatus, current.FileSize, current.ReservedBytes, current.ExpiresAt)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("%s失败: %w", operation, result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		expectedRows := int64(1)
		if current.ReservedBytes > 0 {
			expectedRows++
		}
		if result.RowsAffected != expectedRows {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 读回结果，标记无引用的暂存对象，再统一提交。
		finished, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		if !hasCompletedUploadCleanup(finished, terminalStatus) || finished.StagedObjectID != staged.ID ||
			finished.LeaseVersion != current.LeaseVersion || !finished.ExpiresAt.Equal(current.ExpiresAt) ||
			finished.FileSize != current.FileSize || finished.UploadMode != current.UploadMode ||
			finished.PublishedObjectID != nil || finished.ResultEntryID != nil || finished.ResolvedName != nil {
			return domain.ErrInvalidUploadTaskData
		}
		if _, err := lockedStore.markUnreferencedUploadObjectOrphaned(txContext, staged); err != nil {
			return err
		}
		completed = finished
		return nil
	})
	if err != nil {
		return nil, err
	}
	return completed, nil
}

// hasCompletedUploadCleanup 核对新写入与重复调用共用的清理结果。
func hasCompletedUploadCleanup(task *domain.UploadTask, terminalStatus domain.UploadStatus) bool {
	return task.Status == terminalStatus && task.ReservedBytes == 0 &&
		task.LeaseOwner == nil && task.LeaseUntil == nil && task.NextAttemptAt == nil &&
		task.LastErrorCode == nil && task.LastErrorMessage == nil &&
		task.CompletedAt != nil && task.CompletedAt.Equal(task.UpdatedAt) &&
		(terminalStatus != domain.UploadStatusExpired || !task.CompletedAt.Before(task.ExpiresAt))
}
