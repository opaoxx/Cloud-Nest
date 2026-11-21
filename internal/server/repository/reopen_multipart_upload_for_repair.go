package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm/clause"
)

// ReopenMultipartUploadForRepair 恢复原会话的补传资格，并在同一事务中清除旧冻结回执。
// evidence 由事务外的只读修复检查产生。provider 与 bucket 须来自该检查使用的存储配置。
func (store *Store) ReopenMultipartUploadForRepair(ctx context.Context, provider, bucket string, evidence *domain.UploadMergeRepairEvidence) (*domain.UploadTask, error) {
	// 1. 校验修复证据，锁定账号、任务和暂存对象。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("恢复杂片补传需要数据入口和上下文")
	}
	if evidence == nil || !validMultipartMergeClaim(&evidence.Task) || len(evidence.PartNumbers) == 0 ||
		len(evidence.PartNumbers) > domain.MaxUploadPartsMismatchNumbers {
		return nil, domain.ErrInvalidUploadTaskData
	}
	claimed := &evidence.Task
	var previousNumber uint32
	for _, number := range evidence.PartNumbers {
		if number <= previousNumber || number > *claimed.PartCount {
			return nil, domain.ErrInvalidUploadTaskData
		}
		previousNumber = number
	}
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: evidence.ObjectKey}
	if !location.IsValid() || !validMultipartMergeObjectText(location.ObjectKey) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	var reopened *domain.UploadTask
	err := store.withActiveUserTransaction(ctx, claimed.UserID, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.lockMultipartMergeTask(txContext, claimed)
		if err != nil {
			return err
		}
		if !current.LeaseUntil.Equal(*claimed.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		object, err := lockedStore.FindUserStoredObject(txContext, current.UserID, current.StagedObjectID)
		if err != nil {
			return err
		}
		if !matchesUnwrittenMultipartObject(object, current.FileSize, location) {
			return domain.ErrInvalidStoredObjectData
		}

		// 2. 按当前租约恢复补传，记录固定的失配原因并清除旧回执。
		mismatchMessage := domain.ErrUploadPartsMismatch.Error()
		database := txStore.database.WithContext(txContext)
		changed := database.Exec(`
			UPDATE upload_tasks
			SET status = 'UPLOADING', lease_owner = NULL, lease_until = NULL, next_attempt_at = NULL,
			    last_error_code = ?, last_error_message = ?, updated_at = UTC_TIMESTAMP(6)
			WHERE user_id = ? AND id = ? AND staged_object_id = ? AND status = 'COMPLETING' AND upload_mode = 'MULTIPART'
			  AND CAST(storage_upload_id AS BINARY) = ? AND CAST(lease_owner AS BINARY) = ?
			  AND lease_version = ? AND lease_until = ? AND lease_until > UTC_TIMESTAMP(6)
			  AND expires_at > UTC_TIMESTAMP(6) AND reserved_bytes = file_size`,
			domain.UploadPartsMismatchCode, mismatchMessage, current.UserID, current.ID, current.StagedObjectID, *current.StorageUploadID,
			*current.LeaseOwner, current.LeaseVersion, *current.LeaseUntil)
		if err := txContext.Err(); err != nil {
			return err
		}
		if changed.Error != nil {
			return fmt.Errorf("恢复分片补传失败: %w", changed.Error)
		}
		if changed.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if changed.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}
		deleted := database.Where("task_id = ?", current.ID).Delete(&models.UploadPart{})
		if err := txContext.Err(); err != nil {
			return err
		}
		if deleted.Error != nil {
			return fmt.Errorf("清除失配的分片完成快照失败: %w", deleted.Error)
		}
		if deleted.RowsAffected != int64(*current.PartCount) {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读核对任务、失配原因和回执，提交后返回任务。
		updated, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		if updated.Status != domain.UploadStatusUploading || updated.UploadMode != current.UploadMode || updated.StagedObjectID != current.StagedObjectID ||
			!sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) || updated.LeaseVersion != current.LeaseVersion ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.NextAttemptAt != nil ||
			updated.LastErrorCode == nil || *updated.LastErrorCode != domain.UploadPartsMismatchCode ||
			updated.LastErrorMessage == nil || *updated.LastErrorMessage != mismatchMessage || updated.AttemptCount != current.AttemptCount ||
			updated.UpdatedAt.Before(current.UpdatedAt) || !updated.UpdatedAt.Before(current.ExpiresAt) ||
			!updated.UpdatedAt.Before(*current.LeaseUntil) || !sameMultipartUploadDeclaration(current, updated) {
			return domain.ErrInvalidUploadTaskData
		}
		var remaining int64
		if err := database.Model(&models.UploadPart{}).Where("task_id = ?", current.ID).Count(&remaining).Error; err != nil {
			return fmt.Errorf("复核分片完成快照清除失败: %w", err)
		}
		if remaining != 0 {
			return domain.ErrInvalidUploadTaskData
		}
		reopened = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reopened, nil
}
