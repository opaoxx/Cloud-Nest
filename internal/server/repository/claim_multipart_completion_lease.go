package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ClaimUserMultipartCompletionLease 为当前用户的分片任务领取六十秒核对租约。
// expected 来自按账号读取的任务。resolveStatus 是锁内使用的纯阶段判定。
// 返回的是已提交的 UPLOADING 任务，供调用方在事务外核对回执。
func (store *Store) ClaimUserMultipartCompletionLease(
	ctx context.Context, actor domain.Actor, expected *domain.UploadTask, leaseOwner string,
	resolveStatus func(*domain.UploadTask, uint64, uint64, time.Time) (domain.UploadStatus, error),
) (*domain.UploadTask, error) {
	if store == nil || store.database == nil || ctx == nil || resolveStatus == nil {
		return nil, fmt.Errorf("领取分片完成核对租约需要数据入口、上下文和阶段判定")
	}
	if expected == nil || expected.ID == 0 || expected.StagedObjectID == 0 ||
		expected.StorageUploadID == nil || !domain.ValidMultipartUploadID(*expected.StorageUploadID) ||
		len(leaseOwner) == 0 || len(leaseOwner) > 64 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	for _, character := range leaseOwner {
		if character > 127 {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}
	if expected.UserID != actor.UserID {
		return nil, domain.ErrUploadTaskNotFound
	}
	taskID, objectID, leaseVersion := expected.ID, expected.StagedObjectID, expected.LeaseVersion
	uploadID := *expected.StorageUploadID

	// 1. 在已认证账号事务中加锁读取任务，再用数据库时间复核资格。
	var claimed *domain.UploadTask
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.LockUserUploadTask(txContext, taskID)
		if err != nil {
			return err
		}
		var now time.Time
		clockError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
		if err := txContext.Err(); err != nil {
			return err
		}
		if clockError != nil {
			return fmt.Errorf("读取分片完成核对租约数据库时间失败: %w", clockError)
		}
		now = now.UTC()
		status, err := resolveStatus(current, actor.UserID, taskID, now)
		if err != nil {
			return err
		}
		if status != domain.UploadStatusUploading || current.Status != status || current.UploadMode != "MULTIPART" {
			return domain.ErrUploadTaskNotReady
		}
		if current.StagedObjectID != objectID || current.LeaseVersion != leaseVersion ||
			current.StorageUploadID == nil || *current.StorageUploadID != uploadID {
			return domain.ErrUploadTaskBusy
		}
		if leaseVersion == ^uint64(0) {
			return domain.ErrUploadLeaseVersionExhausted
		}
		if now.Add(time.Minute).Year() > 9999 {
			return domain.ErrInvalidUploadTaskData
		}

		// 2. 按对象、会话、版本和有效期条件写入 60 秒租约。
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET lease_owner = ?, lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND), "+
				"lease_version = lease_version + 1, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND lease_version < ? "+
				"AND status = 'UPLOADING' AND upload_mode = 'MULTIPART' AND CAST(storage_upload_id AS BINARY) = ? "+
				"AND reserved_bytes = file_size AND expires_at = ? AND expires_at > UTC_TIMESTAMP(6) "+
				"AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))",
			leaseOwner, actor.UserID, taskID, objectID, leaseVersion, ^uint64(0), uploadID, current.ExpiresAt)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("领取分片完成核对租约失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskNotReady
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读新租约，等事务提交后再交付。
		updated, err := txStore.LockUserUploadTask(txContext, taskID)
		if errors.Is(err, domain.ErrUploadTaskNotFound) {
			return domain.ErrInvalidUploadTaskData
		}
		if err != nil {
			return err
		}
		if updated.Status != status || updated.UploadMode != current.UploadMode || updated.StagedObjectID != objectID ||
			!sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) || !updated.ExpiresAt.Equal(current.ExpiresAt) ||
			updated.ReservedBytes != current.ReservedBytes || updated.LeaseOwner == nil || *updated.LeaseOwner != leaseOwner ||
			updated.LeaseVersion != leaseVersion+1 || updated.LeaseUntil == nil || !updated.LeaseUntil.Equal(updated.UpdatedAt.Add(time.Minute)) ||
			updated.UpdatedAt.Before(now) || !updated.UpdatedAt.Before(updated.ExpiresAt) {
			return domain.ErrInvalidUploadTaskData
		}
		claimed = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}
