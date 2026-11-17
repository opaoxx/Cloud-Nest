package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecordUserUploadObjectWrite 在已认证用户事务中记录当前接收尝试的存储确认信息。
// 已知元数据保持一致，缺失字段可以补齐，返回对象由外层事务提交确认。
func (store *Store) RecordUserUploadObjectWrite(ctx context.Context, taskID, objectID, leaseVersion uint64, leaseOwner string, confirmation domain.UploadObjectResult) (*domain.StoredObject, error) {
	// 1. 校验已认证事务、标识、租约和写入确认数据。
	if store == nil || ctx == nil || taskID == 0 || objectID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("记录上传对象写入需要上下文、非零任务及对象 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("记录上传对象写入必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if leaseVersion == 0 || len(leaseOwner) == 0 || len(leaseOwner) > 64 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	for _, character := range leaseOwner {
		if character > 127 {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
	confirmation.StorageETag = copyUploadTaskValue(confirmation.StorageETag)
	confirmation.StorageVersionID = copyUploadTaskValue(confirmation.StorageVersionID)
	for _, value := range []*string{confirmation.StorageETag, confirmation.StorageVersionID} {
		if value == nil {
			continue
		}
		if len(*value) > 255 {
			return nil, domain.ErrInvalidStoredObjectData
		}
		for _, character := range *value {
			if character > 127 {
				return nil, domain.ErrInvalidStoredObjectData
			}
		}
	}
	if confirmation.SizeBytes > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}

	// 2. 加锁核对当前任务、暂存对象及接收阶段。
	task, err := store.LockUserUploadTask(writeContext, taskID)
	if err != nil {
		return nil, err
	}
	if task.UploadMode != "SINGLE" || task.Status != domain.UploadStatusUploading {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.FileSize > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}
	if task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil || task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.StagedObjectID != objectID || task.LeaseVersion != leaseVersion || task.LeaseOwner == nil || task.LeaseUntil == nil || *task.LeaseOwner != leaseOwner {
		return nil, domain.ErrUploadTaskBusy
	}
	if confirmation.SizeBytes != task.FileSize {
		return nil, domain.ErrUploadBodySizeMismatch
	}
	lockedStore := *store
	lockedStore.database = store.database.WithContext(writeContext).Clauses(clause.Locking{Strength: "UPDATE"})
	object, err := lockedStore.FindUserStoredObject(writeContext, actor.UserID, objectID)
	if err != nil {
		return nil, err
	}
	if object.Status != domain.StoredObjectStatusStaging || object.FileSize != task.FileSize || object.VerifiedSHA256 != nil ||
		object.DedupHash != nil || object.VerifiedAt != nil || object.UnreferencedAt != nil || object.GCAfter != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 使用数据库时间检查任务期限与租约。
	var databaseTime time.Time
	queryError := store.database.WithContext(writeContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&databaseTime)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取对象写入确认数据库时间失败: %w", queryError)
	}
	databaseTime = databaseTime.UTC()
	if databaseTime.Year() < 1000 || databaseTime.Year() > 9999 || databaseTime.Nanosecond()%1000 != 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if !databaseTime.Before(task.ExpiresAt) {
		return nil, domain.ErrUploadTaskNotReady
	}
	if !databaseTime.Before(*task.LeaseUntil) {
		return nil, domain.ErrUploadTaskBusy
	}

	// 4. 核对已有元数据，准备需要补齐的字段。
	if (object.StorageETag != nil && confirmation.StorageETag != nil && *object.StorageETag != *confirmation.StorageETag) ||
		(object.StorageVersionID != nil && confirmation.StorageVersionID != nil && *object.StorageVersionID != *confirmation.StorageVersionID) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	etag, versionID := object.StorageETag, object.StorageVersionID
	if confirmation.StorageETag != nil {
		etag = confirmation.StorageETag
	}
	if confirmation.StorageVersionID != nil {
		versionID = confirmation.StorageVersionID
	}
	if sameUploadStorageText(object.StorageETag, etag) && sameUploadStorageText(object.StorageVersionID, versionID) {
		if err := writeContext.Err(); err != nil {
			return nil, err
		}
		return object, nil
	}

	// 5. 按任务、对象和有效租约条件保存写入信息。
	result := store.database.WithContext(writeContext).Exec(
		"UPDATE stored_objects AS stored_object JOIN upload_tasks AS upload_task "+
			"ON upload_task.user_id = stored_object.owner_user_id AND upload_task.staged_object_id = stored_object.id "+
			"SET stored_object.storage_etag = ?, stored_object.storage_version_id = ?, stored_object.updated_at = UTC_TIMESTAMP(6) "+
			"WHERE stored_object.owner_user_id = ? AND stored_object.id = ? AND stored_object.status = 'STAGING' "+
			"AND stored_object.file_size = upload_task.file_size AND upload_task.reserved_bytes = upload_task.file_size "+
			"AND upload_task.id = ? AND upload_task.lease_owner = ? AND upload_task.lease_version = ? "+
			"AND upload_task.lease_until > UTC_TIMESTAMP(6) AND upload_task.expires_at > UTC_TIMESTAMP(6) "+
			"AND upload_task.status = 'UPLOADING' AND upload_task.upload_mode = 'SINGLE'",
		etag, versionID, actor.UserID, objectID, taskID, leaseOwner, leaseVersion)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("保存上传对象写入信息失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 6. 当前读复核结果，交由外层事务确认提交。
	saved, err := lockedStore.FindUserStoredObject(writeContext, actor.UserID, objectID)
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err != nil {
		return nil, err
	}
	if saved.Provider != object.Provider || saved.Bucket != object.Bucket || saved.ObjectKey != object.ObjectKey || saved.FileSize != object.FileSize ||
		saved.Status != domain.StoredObjectStatusStaging || !sameUploadStorageText(saved.StorageETag, etag) || !sameUploadStorageText(saved.StorageVersionID, versionID) ||
		saved.VerifiedSHA256 != nil || saved.DedupHash != nil || saved.VerifiedAt != nil || saved.UnreferencedAt != nil || saved.GCAfter != nil ||
		!sameUploadStorageText(saved.LastErrorCode, object.LastErrorCode) || !saved.CreatedAt.Equal(object.CreatedAt) || saved.UpdatedAt.Before(databaseTime) ||
		!saved.UpdatedAt.Before(task.ExpiresAt) || !saved.UpdatedAt.Before(*task.LeaseUntil) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	return saved, nil
}

// sameUploadStorageText 比较可空存储文本，保留 nil 与显式空字符串的差异。
func sameUploadStorageText(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
