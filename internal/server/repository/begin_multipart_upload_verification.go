package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// BeginMultipartUploadVerification 保存已确认的对象元数据，并将任务和暂存对象一起推进 VERIFYING。
// claimed 来自后台合并租约领取，位置与 confirmation 来自同一次存储确认。提交后才返回任务。
func (store *Store) BeginMultipartUploadVerification(
	ctx context.Context, claimed *domain.UploadTask, provider, bucket, objectKey string, confirmation domain.UploadObjectResult,
) (*domain.UploadTask, error) {
	// 1. 检查对象元数据，锁定所属账号、任务和暂存对象。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("推进分片上传至校验阶段需要数据入口和上下文")
	}
	if !validMultipartMergeClaim(claimed) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if confirmation.SizeBytes != claimed.FileSize {
		return nil, domain.ErrUploadBodySizeMismatch
	}
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: objectKey}
	etag, versionID := copyUploadTaskValue(confirmation.StorageETag), copyUploadTaskValue(confirmation.StorageVersionID)
	if !location.IsValid() || !validMultipartMergeObjectText(objectKey) || etag == nil || !validMultipartMergeObjectText(*etag) ||
		(versionID != nil && !validMultipartMergeObjectText(*versionID)) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	var committed *domain.UploadTask
	err := store.withActiveUserTransaction(ctx, claimed.UserID, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.lockMultipartMergeTask(txContext, claimed)
		if err != nil {
			return err
		}
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		object, err := lockedStore.FindUserStoredObject(txContext, current.UserID, current.StagedObjectID)
		if err != nil {
			return err
		}

		// 2. 核对会话、位置及租约，条件写入版本并推进两个状态。
		if !current.LeaseUntil.Equal(*claimed.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		if object.Status != domain.StoredObjectStatusStaging || object.FileSize != current.FileSize ||
			object.Provider != provider || object.Bucket != bucket || object.ObjectKey != objectKey ||
			object.StorageETag != nil || object.StorageVersionID != nil || object.VerifiedAt != nil ||
			object.VerifiedSHA256 != nil || object.DedupHash != nil || object.UnreferencedAt != nil || object.GCAfter != nil || object.LastErrorCode != nil {
			return domain.ErrInvalidStoredObjectData
		}
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE upload_tasks AS task JOIN stored_objects AS object
			ON object.owner_user_id = task.user_id AND object.id = task.staged_object_id
			SET task.status = 'VERIFYING', task.lease_owner = NULL, task.lease_until = NULL,
			    task.next_attempt_at = NULL, task.last_error_code = NULL, task.last_error_message = NULL,
			    task.updated_at = UTC_TIMESTAMP(6), object.updated_at = UTC_TIMESTAMP(6),
			    object.status = 'VERIFYING', object.storage_etag = ?, object.storage_version_id = ?
			WHERE task.user_id = ? AND task.id = ? AND task.staged_object_id = ?
			  AND task.status = 'COMPLETING' AND task.upload_mode = 'MULTIPART'
			  AND CAST(task.storage_upload_id AS BINARY) = ? AND CAST(task.lease_owner AS BINARY) = ?
			  AND task.lease_version = ? AND task.lease_until = ?
			  AND task.lease_until > UTC_TIMESTAMP(6) AND task.expires_at > UTC_TIMESTAMP(6)
			  AND task.reserved_bytes = task.file_size AND object.file_size = task.file_size
			  AND object.status = 'STAGING' AND object.storage_etag IS NULL AND object.storage_version_id IS NULL`,
			etag, versionID, current.UserID, current.ID, object.ID,
			*current.StorageUploadID, *current.LeaseOwner, current.LeaseVersion, *current.LeaseUntil)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("推进分片上传至校验阶段失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 2 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读确认写入结果，事务提交后返回任务。
		updated, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		if updated.Status != domain.UploadStatusVerifying || updated.UploadMode != current.UploadMode || updated.StagedObjectID != object.ID ||
			!sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) || updated.LeaseVersion != current.LeaseVersion ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.NextAttemptAt != nil ||
			updated.LastErrorCode != nil || updated.LastErrorMessage != nil || updated.AttemptCount != current.AttemptCount ||
			updated.UpdatedAt.Before(current.UpdatedAt) || !updated.UpdatedAt.Before(current.ExpiresAt) ||
			!updated.UpdatedAt.Before(*current.LeaseUntil) || !sameMultipartUploadDeclaration(current, updated) {
			return domain.ErrInvalidUploadTaskData
		}
		verifiedObject, err := lockedStore.FindUserStoredObject(txContext, current.UserID, object.ID)
		if err != nil {
			return err
		}
		expectedObject := *object
		expectedObject.Status = domain.StoredObjectStatusVerifying
		expectedObject.StorageETag, expectedObject.StorageVersionID = etag, versionID
		expectedObject.UpdatedAt = updated.UpdatedAt
		if !sameStoredObjectLocation(&expectedObject, verifiedObject) || !sameUploadStorageText(verifiedObject.StorageETag, etag) ||
			verifiedObject.Status != expectedObject.Status || verifiedObject.FileSize != expectedObject.FileSize ||
			verifiedObject.VerifiedAt != nil || verifiedObject.VerifiedSHA256 != nil || verifiedObject.DedupHash != nil ||
			verifiedObject.UnreferencedAt != nil || verifiedObject.GCAfter != nil || verifiedObject.LastErrorCode != nil ||
			!verifiedObject.CreatedAt.Equal(expectedObject.CreatedAt) || !verifiedObject.UpdatedAt.Equal(expectedObject.UpdatedAt) {
			return domain.ErrInvalidStoredObjectData
		}
		committed = updated
		return nil
	})
	if err != nil {
		return nil, markUploadTransactionRetryError(ctx, err, domain.ErrUploadMergeWriteRetryable)
	}
	return committed, nil
}

func validMultipartMergeObjectText(value string) bool {
	return len(value) <= 255 && strings.TrimSpace(value) != "" && !strings.ContainsFunc(value, func(character rune) bool {
		return character < 32 || character > 126
	})
}
