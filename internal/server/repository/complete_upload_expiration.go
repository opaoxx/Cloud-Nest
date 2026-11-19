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
	// 1. 锁定所属用户和任务，复核本次过期处理。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("终结过期上传需要数据入口和上下文")
	}
	if task == nil || task.UserID == 0 || task.ID == 0 || task.StagedObjectID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.UploadMode != "SINGLE" || (task.Status != domain.UploadStatusExpiring && task.Status != domain.UploadStatusExpired) {
		return nil, domain.ErrUploadTaskNotReady
	}

	var expired *domain.UploadTask
	const allowDisabled = true
	err := store.withLockedUserTransaction(ctx, task.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.FindUserUploadTask(txContext, task.UserID, task.ID)
		if err != nil {
			return err
		}
		if current.UploadMode != "SINGLE" || (current.Status != domain.UploadStatusExpiring && current.Status != domain.UploadStatusExpired) {
			return domain.ErrUploadTaskNotReady
		}
		if current.StagedObjectID != task.StagedObjectID || current.LeaseVersion != task.LeaseVersion ||
			!current.ExpiresAt.Equal(task.ExpiresAt) || current.FileSize != task.FileSize {
			return domain.ErrUploadTaskBusy
		}
		if current.PublishedObjectID != nil || current.ResultEntryID != nil || current.ResolvedName != nil {
			return domain.ErrInvalidUploadTaskData
		}
		if current.Status == domain.UploadStatusExpired {
			if !hasCompletedUploadExpiration(current) {
				return domain.ErrInvalidUploadTaskData
			}
			expired = current
			return nil
		}
		if task.Status != domain.UploadStatusExpiring || current.ReservedBytes != task.ReservedBytes || current.CompletedAt != nil {
			return domain.ErrInvalidUploadTaskData
		}

		// 2. 核对账户预占和暂存对象，写入 EXPIRED 并释放预占。
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
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE users AS quota_user JOIN upload_tasks AS upload_task ON upload_task.user_id = quota_user.id "+
				"SET quota_user.reserved_storage = ?, quota_user.updated_at = IF(? = 0, quota_user.updated_at, UTC_TIMESTAMP(6)), "+
				"upload_task.status = 'EXPIRED', upload_task.reserved_bytes = 0, "+
				"upload_task.completed_at = UTC_TIMESTAMP(6), upload_task.updated_at = UTC_TIMESTAMP(6), "+
				"upload_task.lease_owner = NULL, upload_task.lease_until = NULL, upload_task.next_attempt_at = NULL, "+
				"upload_task.last_error_code = NULL, upload_task.last_error_message = NULL "+
				"WHERE quota_user.id = ? AND quota_user.status = ? "+
				"AND quota_user.storage_quota = ? AND quota_user.used_storage = ? AND quota_user.reserved_storage = ? "+
				"AND upload_task.id = ? AND upload_task.staged_object_id = ? AND upload_task.lease_version = ? "+
				"AND upload_task.status = 'EXPIRING' AND upload_task.upload_mode = 'SINGLE' "+
				"AND upload_task.file_size = ? AND upload_task.reserved_bytes = ? "+
				"AND upload_task.expires_at = ? AND upload_task.expires_at <= UTC_TIMESTAMP(6) "+
				"AND (upload_task.lease_until IS NULL OR upload_task.lease_until <= UTC_TIMESTAMP(6)) "+
				"AND (upload_task.next_attempt_at IS NULL OR upload_task.next_attempt_at <= UTC_TIMESTAMP(6)) "+
				"AND upload_task.published_object_id IS NULL AND upload_task.result_entry_id IS NULL "+
				"AND upload_task.resolved_name IS NULL AND upload_task.completed_at IS NULL",
			account.ReservedBytes-current.ReservedBytes, current.ReservedBytes,
			current.UserID, account.Status, account.QuotaBytes, account.UsedBytes, account.ReservedBytes,
			current.ID, staged.ID, current.LeaseVersion, current.FileSize, current.ReservedBytes, current.ExpiresAt)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("终结过期上传失败: %w", result.Error)
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
		if !hasCompletedUploadExpiration(finished) || finished.StagedObjectID != staged.ID ||
			finished.LeaseVersion != current.LeaseVersion || !finished.ExpiresAt.Equal(current.ExpiresAt) ||
			finished.FileSize != current.FileSize || finished.UploadMode != current.UploadMode ||
			finished.PublishedObjectID != nil || finished.ResultEntryID != nil || finished.ResolvedName != nil {
			return domain.ErrInvalidUploadTaskData
		}
		if _, err := lockedStore.markUnreferencedUploadObjectOrphaned(txContext, staged); err != nil {
			return err
		}
		expired = finished
		return nil
	})
	if err != nil {
		// 4. 事务结束后识别可重试的数据库错误。
		return nil, markUploadExpirationRetryError(ctx, err)
	}
	return expired, nil
}

// hasCompletedUploadExpiration 核对新写入与重复调用共用的过期结果。
func hasCompletedUploadExpiration(task *domain.UploadTask) bool {
	return task.Status == domain.UploadStatusExpired && task.ReservedBytes == 0 &&
		task.LeaseOwner == nil && task.LeaseUntil == nil && task.NextAttemptAt == nil &&
		task.LastErrorCode == nil && task.LastErrorMessage == nil &&
		task.CompletedAt != nil && task.CompletedAt.Equal(task.UpdatedAt) && !task.CompletedAt.Before(task.ExpiresAt)
}
