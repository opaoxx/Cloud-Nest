package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ReleaseUserMultipartCompletionLease 释放仍属于该次核对请求的租约。
// claimed 是 ClaimUserMultipartCompletionLease 的结果。已释放或已被替换时返回 false, nil。
func (store *Store) ReleaseUserMultipartCompletionLease(ctx context.Context, actor domain.Actor, claimed *domain.UploadTask) (bool, error) {
	// 1. 检查租约快照，进入已认证账号事务。
	if store == nil || store.database == nil || ctx == nil {
		return false, fmt.Errorf("释放分片完成核对租约需要数据入口和上下文")
	}
	if !validMultipartCompletionLeaseSnapshot(claimed) {
		return false, domain.ErrInvalidUploadTaskData
	}
	if claimed.UserID != actor.UserID {
		return false, domain.ErrUploadTaskNotFound
	}
	taskID, objectID, leaseVersion := claimed.ID, claimed.StagedObjectID, claimed.LeaseVersion
	uploadID, leaseOwner := *claimed.StorageUploadID, *claimed.LeaseOwner

	var released bool
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		// 2. 仅在绑定和租约仍匹配时清空持有者与截止时间。
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET lease_owner = NULL, lease_until = NULL, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND status = 'UPLOADING' AND upload_mode = 'MULTIPART' "+
				"AND CAST(storage_upload_id AS BINARY) = ? AND CAST(lease_owner AS BINARY) = ? "+
				"AND lease_version = ? AND lease_until IS NOT NULL",
			actor.UserID, taskID, objectID, uploadID, leaseOwner, leaseVersion)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("释放分片完成核对租约失败: %w", result.Error)
		}

		// 3. 核对影响行数，提交后返回是否释放。
		if result.RowsAffected > 1 {
			return domain.ErrInvalidUploadTaskData
		}
		released = result.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return released, nil
}

// validMultipartCompletionLeaseSnapshot 检查领取结果里用于识别核对租约的字段。
func validMultipartCompletionLeaseSnapshot(claimed *domain.UploadTask) bool {
	return claimed != nil && claimed.UserID != 0 && claimed.ID != 0 && claimed.StagedObjectID != 0 &&
		claimed.UploadMode == "MULTIPART" && claimed.Status == domain.UploadStatusUploading &&
		claimed.StorageUploadID != nil && domain.ValidMultipartUploadID(*claimed.StorageUploadID) &&
		claimed.LeaseVersion != 0 && claimed.LeaseOwner != nil && len(*claimed.LeaseOwner) > 0 &&
		len(*claimed.LeaseOwner) <= 64 && claimed.LeaseUntil != nil && !claimed.LeaseUntil.IsZero()
}
