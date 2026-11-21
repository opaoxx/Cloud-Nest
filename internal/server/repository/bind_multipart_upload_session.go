package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// BindMultipartUploadSession 将事务外取得的会话 ID 绑定到当前初始化租约。
// claimed 来自初始化租约领取，须用事务外 Store 调用，成功时返回已提交任务。
func (store *Store) BindMultipartUploadSession(ctx context.Context, claimed *domain.UploadTask, uploadID string) (*domain.UploadTask, error) {
	// 1. 校验会话与租约凭据，锁定所属账号并复核任务及暂存对象。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("绑定分片会话需要数据入口和上下文")
	}
	if claimed == nil || claimed.UserID == 0 || claimed.ID == 0 || claimed.StagedObjectID == 0 ||
		claimed.LeaseVersion == 0 || claimed.LeaseOwner == nil || !domain.ValidMultipartUploadID(uploadID) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	userID, taskID, objectID := claimed.UserID, claimed.ID, claimed.StagedObjectID
	leaseOwner, leaseVersion := *claimed.LeaseOwner, claimed.LeaseVersion
	if len(leaseOwner) == 0 || len(leaseOwner) > 64 || strings.ContainsFunc(leaseOwner, func(character rune) bool { return character > 127 }) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	var bound *domain.UploadTask
	err := store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		task, err := lockedStore.FindUserUploadTask(txContext, userID, taskID)
		if err != nil {
			return err
		}
		if task.Status != domain.UploadStatusCreating || task.UploadMode != "MULTIPART" || task.StorageUploadID != nil {
			return domain.ErrUploadTaskNotReady
		}
		if task.StagedObjectID != objectID || task.LeaseVersion != leaseVersion ||
			task.LeaseOwner == nil || *task.LeaseOwner != leaseOwner || task.LeaseUntil == nil {
			return domain.ErrUploadTaskBusy
		}
		if task.ExpectedSHA256 == nil || task.PartSize == nil || task.PartCount == nil ||
			!domain.ValidMultipartUploadLayout(task.FileSize, *task.PartSize, *task.PartCount) ||
			task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil ||
			task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil {
			return domain.ErrInvalidUploadTaskData
		}
		object, err := lockedStore.FindUserStoredObject(txContext, userID, objectID)
		if err != nil {
			return err
		}
		if object.Status != domain.StoredObjectStatusStaging || object.FileSize != task.FileSize ||
			object.StorageVersionID != nil || object.StorageETag != nil || object.VerifiedSHA256 != nil ||
			object.VerifiedAt != nil || object.UnreferencedAt != nil || object.GCAfter != nil || object.LastErrorCode != nil {
			return domain.ErrInvalidStoredObjectData
		}

		// 2. 按数据库时间复核期限，条件保存会话并释放初始化租约。
		var now time.Time
		queryError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
		if err := txContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return fmt.Errorf("读取分片会话绑定数据库时间失败: %w", queryError)
		}
		if !now.Before(task.ExpiresAt) {
			return domain.ErrUploadTaskNotReady
		}
		if !now.Before(*task.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET storage_upload_id = ?, status = 'UPLOADING', "+
				"lease_owner = NULL, lease_until = NULL, next_attempt_at = NULL, "+
				"last_error_code = NULL, last_error_message = NULL, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND status = 'CREATING' "+
				"AND upload_mode = 'MULTIPART' AND storage_upload_id IS NULL AND reserved_bytes = file_size "+
				"AND lease_owner = ? AND lease_version = ? "+
				"AND lease_until > UTC_TIMESTAMP(6) AND expires_at > UTC_TIMESTAMP(6)",
			uploadID, userID, taskID, objectID, leaseOwner, leaseVersion)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("绑定分片存储会话失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读核对状态、会话原值及保留字段，提交后才交付任务。
		updated, err := lockedStore.FindUserUploadTask(txContext, userID, taskID)
		if errors.Is(err, domain.ErrUploadTaskNotFound) {
			return domain.ErrInvalidUploadTaskData
		}
		if err != nil {
			return err
		}
		if updated.Status != domain.UploadStatusUploading || updated.UploadMode != task.UploadMode ||
			updated.StorageUploadID == nil || *updated.StorageUploadID != uploadID ||
			updated.StagedObjectID != objectID || updated.LeaseVersion != leaseVersion ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.NextAttemptAt != nil ||
			updated.LastErrorCode != nil || updated.LastErrorMessage != nil ||
			updated.FileSize != task.FileSize || updated.ReservedBytes != task.ReservedBytes ||
			updated.PartSize == nil || *updated.PartSize != *task.PartSize ||
			updated.PartCount == nil || *updated.PartCount != *task.PartCount ||
			updated.ExpectedSHA256 == nil || *updated.ExpectedSHA256 != *task.ExpectedSHA256 ||
			updated.DeviceID != task.DeviceID || updated.TargetParentID != task.TargetParentID ||
			updated.RequestedName != task.RequestedName || updated.AttemptCount != task.AttemptCount ||
			updated.PublishedObjectID != nil || updated.ResolvedName != nil || updated.ResultEntryID != nil || updated.CompletedAt != nil ||
			!updated.CreatedAt.Equal(task.CreatedAt) || !updated.ExpiresAt.Equal(task.ExpiresAt) ||
			updated.UpdatedAt.Before(now) || !updated.UpdatedAt.Before(task.ExpiresAt) || !updated.UpdatedAt.Before(*task.LeaseUntil) {
			return domain.ErrInvalidUploadTaskData
		}
		bound = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bound, nil
}
