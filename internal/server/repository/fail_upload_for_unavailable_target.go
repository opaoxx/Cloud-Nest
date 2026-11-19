package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FailUploadForUnavailableTarget 终结已确认目标不可用的校验任务，并释放预占。
// 调用方在同一写回复核事务中完成目标检查，并向外传播收尾错误。
func (store *Store) FailUploadForUnavailableTarget(ctx context.Context, task *domain.UploadTask) error {
	// 1. 使用目标不可用的固定原因进入共同的失败收尾。
	return store.failVerifyingUpload(ctx, task, domain.UploadTargetUnavailableCode, domain.ErrUploadTargetUnavailable.Error())
}

// failVerifyingUpload 写入已由调用方确认的校验任务失败结果。
func (store *Store) failVerifyingUpload(ctx context.Context, task *domain.UploadTask, errorCode, errorMessage string) error {
	// 1. 核对当前任务、校验租约、预占和暂存对象。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil {
		return fmt.Errorf("终结校验失败的上传需要上下文和已锁定用户的事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("终结校验失败的上传必须使用事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	userID := store.loginUserID
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.findVerifyingUploadWithFullReservation(ctx, task)
	if err != nil {
		return err
	}
	if current.TargetParentID != task.TargetParentID {
		return domain.ErrUploadTaskBusy
	}
	account, err := store.lockUserUploadQuota(ctx, userID)
	if err != nil {
		return err
	}
	if current.ReservedBytes > account.ReservedBytes {
		return domain.ErrInvalidAccountData
	}
	staged, err := lockedStore.FindUserStoredObject(ctx, userID, current.StagedObjectID)
	if err != nil {
		return err
	}
	if staged.Status != domain.StoredObjectStatusVerifying || staged.FileSize != current.FileSize ||
		staged.UnreferencedAt != nil || staged.VerifiedSHA256 != nil || staged.VerifiedAt != nil {
		return domain.ErrInvalidStoredObjectData
	}

	// 2. 等额释放预占，条件写入失败状态和原因。
	result := store.database.WithContext(ctx).Exec(
		"UPDATE users AS quota_user JOIN upload_tasks AS upload_task ON upload_task.user_id = quota_user.id "+
			"SET quota_user.reserved_storage = ?, "+
			"quota_user.updated_at = IF(? = 0, quota_user.updated_at, UTC_TIMESTAMP(6)), "+
			"upload_task.status = 'FAILED', upload_task.reserved_bytes = 0, "+
			"upload_task.completed_at = UTC_TIMESTAMP(6), upload_task.updated_at = UTC_TIMESTAMP(6), "+
			"upload_task.lease_owner = NULL, upload_task.lease_until = NULL, upload_task.next_attempt_at = NULL, "+
			"upload_task.last_error_code = ?, upload_task.last_error_message = ? "+
			"WHERE quota_user.id = ? AND quota_user.status = 'ACTIVE' "+
			"AND quota_user.storage_quota = ? AND quota_user.used_storage = ? AND quota_user.reserved_storage = ? "+
			"AND upload_task.id = ? AND upload_task.staged_object_id = ? AND upload_task.target_parent_id = ? "+
			"AND CAST(upload_task.lease_owner AS BINARY) = ? AND upload_task.lease_version = ? "+
			"AND upload_task.lease_until > UTC_TIMESTAMP(6) AND upload_task.expires_at > UTC_TIMESTAMP(6) "+
			"AND upload_task.status = 'VERIFYING' AND upload_task.upload_mode = 'SINGLE' "+
			"AND upload_task.file_size = ? AND upload_task.reserved_bytes = upload_task.file_size "+
			"AND upload_task.published_object_id IS NULL AND upload_task.result_entry_id IS NULL "+
			"AND upload_task.resolved_name IS NULL AND upload_task.completed_at IS NULL",
		account.ReservedBytes-current.ReservedBytes, current.ReservedBytes, errorCode, errorMessage,
		userID, account.QuotaBytes, account.UsedBytes, account.ReservedBytes,
		current.ID, current.StagedObjectID, current.TargetParentID, *current.LeaseOwner, current.LeaseVersion, current.FileSize)
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("写入上传任务失败结果: %w", result.Error)
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

	// 3. 复核失败结果，并为无引用的暂存对象开始计时。
	failed, err := lockedStore.FindUserUploadTask(ctx, userID, current.ID)
	if err != nil {
		return err
	}
	if failed.Status != domain.UploadStatusFailed || failed.ReservedBytes != 0 ||
		failed.LeaseOwner != nil || failed.LeaseUntil != nil || failed.LeaseVersion != current.LeaseVersion ||
		failed.StagedObjectID != staged.ID || failed.NextAttemptAt != nil ||
		failed.PublishedObjectID != nil || failed.ResultEntryID != nil || failed.ResolvedName != nil ||
		failed.CompletedAt == nil || !failed.CompletedAt.Equal(failed.UpdatedAt) ||
		!failed.CompletedAt.Before(current.ExpiresAt) || !failed.CompletedAt.Before(*current.LeaseUntil) ||
		failed.LastErrorCode == nil || *failed.LastErrorCode != errorCode ||
		failed.LastErrorMessage == nil || *failed.LastErrorMessage != errorMessage {
		return domain.ErrInvalidUploadTaskData
	}
	_, err = lockedStore.markUnreferencedUploadObjectOrphaned(ctx, staged)
	return err
}
