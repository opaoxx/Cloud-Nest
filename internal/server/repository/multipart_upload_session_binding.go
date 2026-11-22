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
	// 1. 校验扫描目标与存储观察是否指向同一应用对象。
	if store == nil || store.database == nil || ctx == nil {
		return false, fmt.Errorf("复核分片会话绑定需要数据入口和上下文")
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return false, fmt.Errorf("只读复核分片会话绑定须使用事务外的数据入口")
	}
	if target.UserID == 0 || target.TaskID == 0 || target.ObjectID == 0 || !domain.ValidMultipartUploadID(session.UploadID) {
		return false, domain.ErrInvalidUploadTaskData
	}
	owner, validKey := domain.ParseUploadObjectKey(target.ObjectKey)
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: target.ObjectKey}
	if !validKey || owner != target.UserID || session.ObjectKey != target.ObjectKey ||
		!location.IsValid() || !validMultipartMergeObjectText(provider) || !validMultipartMergeObjectText(bucket) {
		return false, domain.ErrInvalidStoredObjectData
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return false, err
	}

	// 2. 在同一次只读查询里核对归属、原暂存记录和当前绑定。
	var row struct{ StorageUploadID *string }
	queryError := store.database.WithContext(queryContext).Table("upload_tasks AS task").
		Select("task.storage_upload_id").
		Joins("JOIN stored_objects AS original ON original.id = task.staged_object_id AND original.owner_user_id = task.user_id").
		Where("task.id = ? AND task.user_id = ? AND task.staged_object_id = ? AND task.upload_mode = 'MULTIPART'",
			target.TaskID, target.UserID, target.ObjectID).
		Where("CAST(original.provider AS BINARY) = ? AND CAST(original.bucket AS BINARY) = ? AND CAST(original.object_key AS BINARY) = ?",
			provider, bucket, target.ObjectKey).
		Take(&row).Error
	if err := queryContext.Err(); err != nil {
		return false, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return false, domain.ErrUploadTaskNotFound
	}
	if queryError != nil {
		return false, fmt.Errorf("复核分片会话绑定失败: %w", queryError)
	}

	// 3. 按原文比较会话 ID。任务终态仍保留原绑定，供保护该会话。
	if row.StorageUploadID == nil {
		return false, nil
	}
	if !domain.ValidMultipartUploadID(*row.StorageUploadID) {
		return false, domain.ErrInvalidUploadTaskData
	}
	return *row.StorageUploadID == session.UploadID, nil
}
