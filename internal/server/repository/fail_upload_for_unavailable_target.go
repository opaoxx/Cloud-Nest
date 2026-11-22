package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FailUploadForUnavailableTarget 终结已确认目标不可用的校验任务，并释放预占。
// 调用方在同一写回复核事务中完成目标检查，并向外传播收尾错误。
func (store *Store) FailUploadForUnavailableTarget(ctx context.Context, task *domain.UploadTask) error {
	// 1. 使用目标不可用的固定原因进入共同的失败收尾。
	return store.failVerifyingUpload(ctx, task, domain.UploadTargetUnavailableCode, domain.ErrUploadTargetUnavailable.Error(), nil)
}

// FailUploadReplacementForRevisionConflict 终结仍持有校验租约的替换任务，保存冲突时的版本。
// 调用方须在替换写入前确认冲突，并将收尾失败交给外层事务回滚。
func (store *Store) FailUploadReplacementForRevisionConflict(ctx context.Context, task *domain.UploadTask, currentRevision uint64) error {
	return store.failVerifyingUpload(ctx, task, domain.UploadRevisionConflictCode, domain.ErrEntryRevisionConflict.Error(), &currentRevision)
}

// failVerifyingUpload 写入已由调用方确认的校验任务失败结果。
func (store *Store) failVerifyingUpload(ctx context.Context, task *domain.UploadTask, errorCode, errorMessage string, conflictingRevision *uint64) error {
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
	// 两种上传模式共用已确认失败的写回路径。
	uploadMode := "SINGLE"
	if task != nil && task.UploadMode == "MULTIPART" &&
		(errorCode == domain.UploadTargetUnavailableCode || errorCode == domain.UploadBodySizeMismatchCode ||
			errorCode == domain.UploadChecksumMismatchCode || errorCode == domain.UploadRevisionConflictCode) {
		uploadMode = "MULTIPART"
	}
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.findVerifyingUploadWithFullReservation(ctx, task, uploadMode)
	if err != nil {
		return err
	}
	if current.TargetParentID != task.TargetParentID {
		return domain.ErrUploadTaskBusy
	}
	if conflictingRevision != nil {
		if err := store.checkUploadReplacementConflict(ctx, task, current, *conflictingRevision); err != nil {
			return err
		}
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
	if uploadMode == "MULTIPART" && (!validMultipartMergeObjectText(staged.ObjectKey) || staged.StorageETag == nil ||
		!validMultipartMergeObjectText(*staged.StorageETag) ||
		(staged.StorageVersionID != nil && !validMultipartMergeObjectText(*staged.StorageVersionID))) {
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
			"upload_task.last_error_code = ?, upload_task.last_error_message = ?, upload_task.replacement_conflicting_revision = ? "+
			"WHERE quota_user.id = ? AND quota_user.status = 'ACTIVE' "+
			"AND quota_user.storage_quota = ? AND quota_user.used_storage = ? AND quota_user.reserved_storage = ? "+
			"AND upload_task.id = ? AND upload_task.staged_object_id = ? AND upload_task.target_parent_id = ? "+
			"AND CAST(upload_task.lease_owner AS BINARY) = ? AND upload_task.lease_version = ? "+
			"AND upload_task.lease_until > UTC_TIMESTAMP(6) AND upload_task.expires_at > UTC_TIMESTAMP(6) "+
			"AND upload_task.status = 'VERIFYING' AND upload_task.upload_mode = ? "+
			"AND CAST(upload_task.storage_upload_id AS BINARY) <=> ? "+
			"AND upload_task.file_size = ? AND upload_task.reserved_bytes = upload_task.file_size "+
			"AND upload_task.published_object_id IS NULL AND upload_task.result_entry_id IS NULL "+
			"AND upload_task.resolved_name IS NULL AND upload_task.completed_at IS NULL",
		account.ReservedBytes-current.ReservedBytes, current.ReservedBytes, errorCode, errorMessage, conflictingRevision,
		userID, account.QuotaBytes, account.UsedBytes, account.ReservedBytes,
		current.ID, current.StagedObjectID, current.TargetParentID, *current.LeaseOwner, current.LeaseVersion,
		current.UploadMode, current.StorageUploadID, current.FileSize)
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
	if failed.Status != domain.UploadStatusFailed || failed.UploadMode != current.UploadMode || failed.ReservedBytes != 0 ||
		failed.LeaseOwner != nil || failed.LeaseUntil != nil || failed.LeaseVersion != current.LeaseVersion ||
		failed.StagedObjectID != staged.ID || failed.NextAttemptAt != nil ||
		failed.PublishedObjectID != nil || failed.ResultEntryID != nil || failed.ResolvedName != nil ||
		failed.CompletedAt == nil || !failed.CompletedAt.Equal(failed.UpdatedAt) ||
		!failed.CompletedAt.Before(current.ExpiresAt) || !failed.CompletedAt.Before(*current.LeaseUntil) ||
		failed.LastErrorCode == nil || *failed.LastErrorCode != errorCode ||
		failed.LastErrorMessage == nil || *failed.LastErrorMessage != errorMessage {
		return domain.ErrInvalidUploadTaskData
	}
	if uploadMode == "MULTIPART" && (!sameMultipartUploadPlan(current, failed) || !sameUploadStorageText(failed.StorageUploadID, current.StorageUploadID)) {
		return domain.ErrInvalidUploadTaskData
	}
	if conflictingRevision != nil && (failed.ReplacementEntryID == nil || *failed.ReplacementEntryID != *current.ReplacementEntryID ||
		failed.ReplacementExpectedRevision == nil || *failed.ReplacementExpectedRevision != *current.ReplacementExpectedRevision ||
		failed.ReplacementConflictingRevision == nil || *failed.ReplacementConflictingRevision != *conflictingRevision) {
		return domain.ErrInvalidUploadTaskData
	}
	orphaned, err := lockedStore.markUnreferencedUploadObjectOrphaned(ctx, staged)
	if err != nil {
		return err
	}
	if uploadMode == "MULTIPART" && (!sameStoredObjectLocation(orphaned, staged) || !sameUploadStorageText(orphaned.StorageETag, staged.StorageETag)) {
		return domain.ErrInvalidStoredObjectData
	}
	return nil
}

// checkUploadReplacementConflict 确认冲突版本属于当前任务绑定的可见文件。
func (store *Store) checkUploadReplacementConflict(ctx context.Context, snapshot, current *domain.UploadTask, revision uint64) error {
	// 1. 核对原绑定以及冲突版本的范围。
	if revision == 0 || snapshot.ReplacementEntryID == nil || current.ReplacementEntryID == nil ||
		*snapshot.ReplacementEntryID != *current.ReplacementEntryID ||
		snapshot.ReplacementExpectedRevision == nil || current.ReplacementExpectedRevision == nil ||
		*snapshot.ReplacementExpectedRevision != *current.ReplacementExpectedRevision {
		return domain.ErrInvalidUploadTaskData
	}

	// 2. 沿用目标的可见性和版本锁读，防止用错误证据结束任务。
	_, err := store.LockUploadReplacementTarget(ctx, *current.ReplacementEntryID, *current.ReplacementExpectedRevision)
	var conflict *domain.EntryRevisionConflictError
	if errors.As(err, &conflict) {
		if conflict.CurrentRevision == revision {
			return nil
		}
		return domain.ErrUploadTaskBusy
	}
	if err != nil {
		return err
	}
	return domain.ErrUploadTaskBusy
}
