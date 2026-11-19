package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// PromoteVerifiedUploadObject 在发布事务内将当前暂存对象推进到 READY。
// task 和 staged 来自写回复核回调，调用方先查询复用对象，写入结果由外层事务确认。
func (store *Store) PromoteVerifiedUploadObject(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest) (*domain.StoredObject, error) {
	// 1. 核对事务、校验快照和实测摘要。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil {
		return nil, fmt.Errorf("推进已校验对象需要上下文和已锁定用户的事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("推进已校验对象必须使用事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if task == nil || task.ID == 0 || task.UserID != store.loginUserID || task.StagedObjectID == 0 ||
		task.LeaseVersion == 0 || task.LeaseOwner == nil || *task.LeaseOwner == "" || task.LeaseUntil == nil ||
		task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil || task.ResultEntryID != nil ||
		task.ResolvedName != nil || task.CompletedAt != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.Status != domain.UploadStatusVerifying || task.UploadMode != "SINGLE" {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.FileSize > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}
	if digest.SizeBytes != task.FileSize {
		return nil, domain.ErrUploadBodySizeMismatch
	}
	if task.ExpectedSHA256 != nil && *task.ExpectedSHA256 != digest.SHA256 {
		return nil, domain.ErrUploadChecksumMismatch
	}
	if staged == nil || staged.ID != task.StagedObjectID || staged.OwnerUserID != task.UserID ||
		staged.Status != domain.StoredObjectStatusVerifying || staged.FileSize != task.FileSize ||
		staged.VerifiedSHA256 != nil || staged.DedupHash != nil || staged.VerifiedAt != nil ||
		staged.UnreferencedAt != nil || staged.GCAfter != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 2. 按当前对象、租约和数据库时间写入 READY。
	result := store.database.WithContext(ctx).Exec(
		"UPDATE stored_objects AS stored_object JOIN upload_tasks AS upload_task "+
			"ON upload_task.user_id = stored_object.owner_user_id AND upload_task.staged_object_id = stored_object.id "+
			"SET stored_object.status = 'READY', stored_object.verified_sha256 = ?, "+
			"stored_object.verified_at = UTC_TIMESTAMP(6), stored_object.updated_at = UTC_TIMESTAMP(6), stored_object.last_error_code = NULL "+
			"WHERE stored_object.owner_user_id = ? AND stored_object.id = ? AND stored_object.status = 'VERIFYING' "+
			"AND stored_object.file_size = ? AND stored_object.file_size = upload_task.file_size "+
			"AND stored_object.verified_sha256 IS NULL AND stored_object.verified_at IS NULL "+
			"AND stored_object.unreferenced_at IS NULL AND stored_object.gc_after IS NULL "+
			"AND CAST(stored_object.provider AS BINARY) = ? AND CAST(stored_object.bucket AS BINARY) = ? "+
			"AND CAST(stored_object.object_key AS BINARY) = ? AND CAST(stored_object.storage_version_id AS BINARY) <=> ? "+
			"AND CAST(stored_object.storage_etag AS BINARY) <=> ? "+
			"AND upload_task.id = ? AND CAST(upload_task.lease_owner AS BINARY) = ? AND upload_task.lease_version = ? "+
			"AND upload_task.lease_until > UTC_TIMESTAMP(6) AND upload_task.expires_at > UTC_TIMESTAMP(6) "+
			"AND upload_task.status = 'VERIFYING' AND upload_task.upload_mode = 'SINGLE' "+
			"AND upload_task.reserved_bytes = upload_task.file_size AND upload_task.published_object_id IS NULL "+
			"AND upload_task.result_entry_id IS NULL AND upload_task.resolved_name IS NULL AND upload_task.completed_at IS NULL "+
			"AND (upload_task.expected_sha256 IS NULL OR upload_task.expected_sha256 = ?)",
		digest.SHA256[:], task.UserID, staged.ID, digest.SizeBytes, staged.Provider, staged.Bucket, staged.ObjectKey,
		staged.StorageVersionID, staged.StorageETag, task.ID, *task.LeaseOwner, task.LeaseVersion, digest.SHA256[:])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			// 本次写入只会改变内容去重唯一键，交给调用方重新选择复用对象。
			return nil, domain.ErrStoredObjectDedupConflict
		}
		return nil, fmt.Errorf("推进已校验对象失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, domain.ErrUploadTaskBusy
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 读回复核状态、摘要和存储位置，交由外层事务确认。
	ready, err := store.FindUserStoredObject(ctx, task.UserID, staged.ID)
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err != nil {
		return nil, err
	}
	if ready.Status != domain.StoredObjectStatusReady || ready.VerifiedSHA256 == nil || *ready.VerifiedSHA256 != digest.SHA256 ||
		ready.VerifiedAt == nil || !ready.VerifiedAt.Equal(ready.UpdatedAt) ||
		ready.FileSize != digest.SizeBytes || ready.Provider != staged.Provider || ready.Bucket != staged.Bucket ||
		ready.ObjectKey != staged.ObjectKey || !sameUploadStorageText(ready.StorageVersionID, staged.StorageVersionID) ||
		!sameUploadStorageText(ready.StorageETag, staged.StorageETag) || !ready.CreatedAt.Equal(staged.CreatedAt) ||
		ready.UnreferencedAt != nil || ready.GCAfter != nil || ready.LastErrorCode != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}
	return ready, nil
}
