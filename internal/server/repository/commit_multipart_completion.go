package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// CommitUserMultipartCompletion 保存已核对的回执和完成阶段，返回同一事务提交后的任务。
// claimed 来自完成核对租约领取，receipts 来自事务外对存储回执的完整核对。
func (store *Store) CommitUserMultipartCompletion(
	ctx context.Context, actor domain.Actor, claimed *domain.UploadTask, receipts []domain.UploadPartReceipt,
) (*domain.UploadTask, error) {
	// 1. 校验回执完整性，锁定任务并复查会话和租约。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("保存分片完成快照需要数据入口和上下文")
	}
	if !validMultipartCompletionLeaseSnapshot(claimed) || claimed.PartSize == nil || claimed.PartCount == nil ||
		claimed.ExpectedSHA256 == nil || claimed.ReservedBytes != claimed.FileSize {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if claimed.UserID != actor.UserID {
		return nil, domain.ErrUploadTaskNotFound
	}
	rows, err := newMultipartCompletionRows(claimed, receipts)
	if err != nil {
		return nil, err
	}
	taskID, objectID, leaseVersion := claimed.ID, claimed.StagedObjectID, claimed.LeaseVersion
	uploadID, leaseOwner := *claimed.StorageUploadID, *claimed.LeaseOwner
	var committed *domain.UploadTask
	err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.LockUserUploadTask(txContext, taskID)
		if err != nil {
			return err
		}
		if current.Status != domain.UploadStatusUploading || current.UploadMode != "MULTIPART" {
			return domain.ErrUploadTaskNotReady
		}
		if current.StagedObjectID != objectID || !sameUploadStorageText(current.StorageUploadID, claimed.StorageUploadID) ||
			current.LeaseVersion != leaseVersion || !sameUploadStorageText(current.LeaseOwner, claimed.LeaseOwner) || current.LeaseUntil == nil {
			return domain.ErrUploadTaskBusy
		}
		database := txStore.database.WithContext(txContext)
		var observedAt time.Time
		if err := database.Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&observedAt); err != nil {
			return fmt.Errorf("读取分片完成快照时间失败: %w", err)
		}
		observedAt = observedAt.UTC()
		if !observedAt.Before(current.ExpiresAt) {
			return domain.ErrUploadTaskNotReady
		}
		if !observedAt.Before(*current.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		if !sameMultipartUploadDeclaration(claimed, current) {
			return domain.ErrInvalidUploadTaskData
		}

		// 2. 写入完整回执快照，按有效租约推进为 COMPLETING。
		var existingRows int64
		if err := database.Model(&models.UploadPart{}).Where("task_id = ?", taskID).Count(&existingRows).Error; err != nil {
			return fmt.Errorf("检查分片完成快照失败: %w", err)
		}
		if existingRows != 0 {
			return domain.ErrInvalidUploadTaskData
		}
		for index := range rows {
			rows[index].ObservedAt = observedAt
		}
		insert := database.CreateInBatches(&rows, 500)
		if insert.Error != nil {
			return fmt.Errorf("写入分片完成快照失败: %w", insert.Error)
		}
		if insert.RowsAffected != int64(len(rows)) {
			return domain.ErrInvalidUploadTaskData
		}
		advance := database.Exec(
			"UPDATE upload_tasks SET status = 'COMPLETING', lease_owner = NULL, lease_until = NULL, "+
				"next_attempt_at = NULL, last_error_code = NULL, last_error_message = NULL, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND upload_mode = 'MULTIPART' AND status = 'UPLOADING' "+
				"AND CAST(storage_upload_id AS BINARY) = ? AND CAST(lease_owner AS BINARY) = ? "+
				"AND lease_version = ? AND lease_until > UTC_TIMESTAMP(6) AND expires_at > UTC_TIMESTAMP(6) AND reserved_bytes = file_size",
			actor.UserID, taskID, objectID, uploadID, leaseOwner, leaseVersion)
		if advance.Error != nil {
			return fmt.Errorf("推进分片上传至完成阶段失败: %w", advance.Error)
		}
		if advance.RowsAffected == 0 {
			return domain.ErrUploadTaskNotReady
		}
		if advance.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读任务，事务提交后再交付。
		updated, err := txStore.LockUserUploadTask(txContext, taskID)
		if err != nil {
			return err
		}
		if updated.UploadMode != "MULTIPART" || updated.Status != domain.UploadStatusCompleting || updated.StagedObjectID != objectID ||
			!sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) || updated.LeaseVersion != leaseVersion ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.NextAttemptAt != nil ||
			updated.LastErrorCode != nil || updated.LastErrorMessage != nil || updated.AttemptCount != current.AttemptCount ||
			updated.UpdatedAt.Before(observedAt) || !updated.UpdatedAt.Before(current.ExpiresAt) ||
			!updated.UpdatedAt.Before(*current.LeaseUntil) || !sameMultipartUploadDeclaration(current, updated) {
			return domain.ErrInvalidUploadTaskData
		}
		committed = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return committed, nil
}

// newMultipartCompletionRows 校验固定计划及连续回执，保留原始 ETag 和 SHA-256。
func newMultipartCompletionRows(task *domain.UploadTask, receipts []domain.UploadPartReceipt) ([]models.UploadPart, error) {
	if !domain.ValidMultipartUploadLayout(task.FileSize, *task.PartSize, *task.PartCount) || len(receipts) != int(*task.PartCount) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	algorithm := "SHA256"
	rows := make([]models.UploadPart, len(receipts))
	for index, receipt := range receipts {
		number := uint32(index + 1)
		size, err := domain.ExpectedMultipartUploadPartSize(task.FileSize, *task.PartSize, *task.PartCount, number)
		if err != nil || receipt.PartNumber != number || receipt.SizeBytes != size ||
			!domain.ValidUploadPartETag(receipt.ETag) || !domain.ValidUploadPartChecksumSHA256(receipt.ChecksumSHA256) {
			return nil, domain.ErrInvalidUploadTaskData
		}
		rows[index] = models.UploadPart{TaskID: task.ID, PartNumber: number, ETag: receipt.ETag,
			SizeBytes: size, ChecksumAlgorithm: &algorithm, ChecksumValue: new(receipt.ChecksumSHA256)}
	}
	return rows, nil
}
