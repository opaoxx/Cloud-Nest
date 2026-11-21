package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CompleteExistingMultipartUploadCancellation 终结对象已生成的分片取消任务。
// 在同一次提交中返还预占，并保留观察到的原对象定位供延迟回收。
func (store *Store) CompleteExistingMultipartUploadCancellation(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCancellationObservation,
) (*domain.UploadTask, error) {
	// 1. 确认收到存在对象的观察。元数据及一致性由锁内复核检查。
	if observation == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if observation.Object == nil {
		return nil, domain.ErrUploadTaskNotReady
	}
	// 2. 复用结算与延迟回收事务，只在提交后返回结果。
	return store.completeMultipartUploadCancellation(ctx, provider, bucket, observation)
}

// saveMultipartCancellationObjectLocation 在已持有对象锁时补齐待回收对象的原版本和 ETag。
// 调用方仍须在同一事务中登记回收并核对保存的原值。
func (store *Store) saveMultipartCancellationObjectLocation(
	ctx context.Context, staged *domain.StoredObject, observed *domain.UploadObjectResult,
) error {
	// 1. 只为尚未记录存储结果的暂存对象补齐定位。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE stored_objects
		SET storage_etag = ?, storage_version_id = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE owner_user_id = ? AND id = ? AND status = 'STAGING' AND file_size = ?
		  AND storage_etag IS NULL AND storage_version_id IS NULL
		  AND unreferenced_at IS NULL AND gc_after IS NULL`,
		observed.StorageETag, observed.StorageVersionID, staged.OwnerUserID, staged.ID, staged.FileSize)
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("保存分片取消的对象定位失败: %w", result.Error)
	}
	// 2. 只有原记录完整匹配时才继续登记回收。
	if result.RowsAffected != 1 {
		return domain.ErrInvalidStoredObjectData
	}
	return nil
}
