package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// BeginUserUploadVerification 保存当前写入确认，将任务和对象共同推进 VERIFYING。
// 使用已认证用户事务，回调须返回错误，成功结果由外层提交确认。
func (store *Store) BeginUserUploadVerification(ctx context.Context, taskID, objectID, leaseVersion uint64, leaseOwner string, confirmation domain.UploadObjectResult) (*domain.UploadTask, error) {
	// 1. 保存并复核当前接收尝试的存储确认信息。
	if ctx == nil {
		return nil, fmt.Errorf("进入上传校验阶段需要上下文")
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	object, err := store.RecordUserUploadObjectWrite(writeContext, taskID, objectID, leaseVersion, leaseOwner, confirmation)
	if err != nil {
		return nil, err
	}

	// 2. 读取已锁定任务，核对当前对象和租约。
	task, err := store.LockUserUploadTask(writeContext, taskID)
	if err != nil {
		return nil, err
	}
	if task.StagedObjectID != objectID || task.Status != domain.UploadStatusUploading || task.UploadMode != "SINGLE" ||
		task.FileSize != object.FileSize || task.ReservedBytes != task.FileSize || task.LeaseVersion != leaseVersion ||
		task.LeaseOwner == nil || *task.LeaseOwner != leaseOwner || task.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 按有效接收租约将任务和对象共同推进校验阶段。
	result := store.database.WithContext(writeContext).Exec(
		"UPDATE upload_tasks AS upload_task JOIN stored_objects AS stored_object "+
			"ON stored_object.owner_user_id = upload_task.user_id AND stored_object.id = upload_task.staged_object_id "+
			"SET upload_task.status = 'VERIFYING', upload_task.lease_owner = NULL, upload_task.lease_until = NULL, "+
			"upload_task.next_attempt_at = NULL, upload_task.last_error_code = NULL, upload_task.last_error_message = NULL, "+
			"upload_task.updated_at = UTC_TIMESTAMP(6), stored_object.status = 'VERIFYING', "+
			"stored_object.last_error_code = NULL, stored_object.updated_at = UTC_TIMESTAMP(6) "+
			"WHERE upload_task.user_id = ? AND upload_task.id = ? AND upload_task.staged_object_id = ? "+
			"AND upload_task.lease_owner = ? AND upload_task.lease_version = ? "+
			"AND upload_task.lease_until > UTC_TIMESTAMP(6) AND upload_task.expires_at > UTC_TIMESTAMP(6) "+
			"AND upload_task.status = 'UPLOADING' AND upload_task.upload_mode = 'SINGLE' "+
			"AND upload_task.reserved_bytes = upload_task.file_size AND stored_object.file_size = upload_task.file_size "+
			"AND stored_object.status = 'STAGING'",
		task.UserID, taskID, objectID, leaseOwner, leaseVersion)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("进入上传校验阶段失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 2 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 4. 当前读复核新状态、租约释放和存储定位信息。
	verifying, err := store.LockUserUploadTask(writeContext, taskID)
	if errors.Is(err, domain.ErrUploadTaskNotFound) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err != nil {
		return nil, err
	}
	if verifying.Status != domain.UploadStatusVerifying || verifying.UploadMode != "SINGLE" || verifying.StagedObjectID != objectID ||
		verifying.LeaseOwner != nil || verifying.LeaseUntil != nil || verifying.LeaseVersion != leaseVersion ||
		verifying.NextAttemptAt != nil || verifying.LastErrorCode != nil || verifying.LastErrorMessage != nil ||
		verifying.FileSize != task.FileSize || verifying.ReservedBytes != task.ReservedBytes || verifying.DeviceID != task.DeviceID ||
		verifying.TargetParentID != task.TargetParentID || verifying.RequestedName != task.RequestedName ||
		verifying.PublishedObjectID != nil || verifying.ResolvedName != nil || verifying.ResultEntryID != nil || verifying.CompletedAt != nil ||
		!verifying.ExpiresAt.Equal(task.ExpiresAt) || !verifying.CreatedAt.Equal(task.CreatedAt) || verifying.AttemptCount != task.AttemptCount ||
		verifying.UpdatedAt.Before(object.UpdatedAt) || !verifying.UpdatedAt.Before(task.ExpiresAt) || !verifying.UpdatedAt.Before(*task.LeaseUntil) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if (verifying.ExpectedSHA256 == nil) != (task.ExpectedSHA256 == nil) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.ExpectedSHA256 != nil && *verifying.ExpectedSHA256 != *task.ExpectedSHA256 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	lockedStore := *store
	lockedStore.database = store.database.WithContext(writeContext).Clauses(clause.Locking{Strength: "UPDATE"})
	verifyingObject, err := lockedStore.FindUserStoredObject(writeContext, task.UserID, objectID)
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err != nil {
		return nil, err
	}
	if verifyingObject.Status != domain.StoredObjectStatusVerifying || verifyingObject.Provider != object.Provider ||
		verifyingObject.Bucket != object.Bucket || verifyingObject.ObjectKey != object.ObjectKey || verifyingObject.FileSize != object.FileSize ||
		!sameUploadStorageText(verifyingObject.StorageETag, object.StorageETag) || !sameUploadStorageText(verifyingObject.StorageVersionID, object.StorageVersionID) ||
		verifyingObject.VerifiedSHA256 != nil || verifyingObject.DedupHash != nil || verifyingObject.VerifiedAt != nil ||
		verifyingObject.UnreferencedAt != nil || verifyingObject.GCAfter != nil || verifyingObject.LastErrorCode != nil ||
		!verifyingObject.CreatedAt.Equal(object.CreatedAt) || !verifyingObject.UpdatedAt.Equal(verifying.UpdatedAt) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 5. 返回任务，由外层事务确认提交。
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	return verifying, nil
}
