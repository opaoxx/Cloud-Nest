package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// IsMultipartUploadSessionBound 核对存储观察中的会话是否仍绑定在同一任务。
// false 且无错误只说明当前绑定不同或为空，后续清理仍须复核宽限期和创建租约。
func (store *Store) IsMultipartUploadSessionBound(
	ctx context.Context, provider, bucket string, target domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession,
) (bool, error) {
	row, err := store.readMultipartUploadSessionBinding(ctx, provider, bucket, target, session)
	if err != nil {
		return false, err
	}
	return sameUploadStorageText(row.StorageUploadID, &session.UploadID), nil
}

// ReadMultipartUploadSessionProtection 在同一快照复核绑定和有效创建租约。
// 两项都为 false 时仍需检查会话宽限期，并在处理前重新复核。
func (store *Store) ReadMultipartUploadSessionProtection(
	ctx context.Context, provider, bucket string, target domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession,
) (domain.MultipartUploadSessionProtection, error) {
	// 1. 按同一原对象读取当前绑定、创建租约和数据库时间。
	row, err := store.readMultipartUploadSessionBinding(ctx, provider, bucket, target, session)
	if err != nil {
		return domain.MultipartUploadSessionProtection{}, err
	}

	// 2. 保留已绑定的会话，识别仍有效的创建租约。
	protection := domain.MultipartUploadSessionProtection{Bound: sameUploadStorageText(row.StorageUploadID, &session.UploadID)}
	if row.Status != domain.UploadStatusCreating || row.StorageUploadID != nil || (row.LeaseOwner == nil && row.LeaseUntil == nil) {
		return protection, nil
	}
	if row.LeaseOwner == nil || row.LeaseUntil == nil || row.LeaseVersion == 0 || !validMultipartMergeObjectText(*row.LeaseOwner) ||
		row.DatabaseTime.IsZero() || row.ExpiresAt.IsZero() || row.LeaseUntil.IsZero() {
		return domain.MultipartUploadSessionProtection{}, domain.ErrInvalidUploadTaskData
	}

	// 3. 数据库时间到达任一截止点时，创建租约不再提供保护。
	protection.ActiveCreationLease = row.DatabaseTime.Before(*row.LeaseUntil) && row.DatabaseTime.Before(row.ExpiresAt)
	return protection, nil
}

type multipartUploadSessionBindingRow struct {
	StorageUploadID *string
	Status          domain.UploadStatus
	LeaseOwner      *string
	LeaseVersion    uint64
	LeaseUntil      *time.Time
	ExpiresAt       time.Time
	DatabaseTime    time.Time
}

func (store *Store) readMultipartUploadSessionBinding(
	ctx context.Context, provider, bucket string, target domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession,
) (*multipartUploadSessionBindingRow, error) {
	// 1. 校验扫描目标与存储观察是否指向同一应用对象。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("复核分片会话绑定需要数据入口和上下文")
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("只读复核分片会话绑定须使用事务外的数据入口")
	}
	if target.UserID == 0 || target.TaskID == 0 || target.ObjectID == 0 || !domain.ValidMultipartUploadID(session.UploadID) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	owner, validKey := domain.ParseUploadObjectKey(target.ObjectKey)
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: target.ObjectKey}
	if !validKey || owner != target.UserID || session.ObjectKey != target.ObjectKey ||
		!location.IsValid() || !validMultipartMergeObjectText(provider) || !validMultipartMergeObjectText(bucket) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 在同一次只读查询里核对原暂存记录，读取绑定与租约。
	var row multipartUploadSessionBindingRow
	queryError := store.database.WithContext(queryContext).Table("upload_tasks AS task").
		Select("task.storage_upload_id, task.status, task.lease_owner, task.lease_version, task.lease_until, task.expires_at, UTC_TIMESTAMP(6) AS database_time").
		Joins("JOIN stored_objects AS original ON original.id = task.staged_object_id AND original.owner_user_id = task.user_id").
		Where("task.id = ? AND task.user_id = ? AND task.staged_object_id = ? AND task.upload_mode = 'MULTIPART'",
			target.TaskID, target.UserID, target.ObjectID).
		Where("CAST(original.provider AS BINARY) = ? AND CAST(original.bucket AS BINARY) = ? AND CAST(original.object_key AS BINARY) = ?",
			provider, bucket, target.ObjectKey).
		Take(&row).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrUploadTaskNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("复核分片会话绑定失败: %w", queryError)
	}

	// 3. 保留原始会话 ID，异常绑定不得当作空值。
	if row.StorageUploadID != nil && !domain.ValidMultipartUploadID(*row.StorageUploadID) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return &row, nil
}
