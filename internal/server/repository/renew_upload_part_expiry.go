package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RenewUserUploadPartExpiry 在已认证用户事务中条件续期，提交后返回任务。
// resolveExpiry 使用锁内事实执行纯判断，调用方传入服务层续期规则。
func (store *Store) RenewUserUploadPartExpiry(
	ctx context.Context, actor domain.Actor, taskID uint64, expectedExpiry time.Time,
	resolveExpiry func(*domain.UploadTask, *domain.UserProfile, time.Time, time.Time) (time.Time, error),
) (*domain.UploadTask, error) {
	// 1. 复查账号、设备和会话，锁定当前用户的任务及配额。
	if store == nil || store.database == nil || ctx == nil || taskID == 0 || resolveExpiry == nil {
		return nil, fmt.Errorf("续期分片任务需要数据入口、上下文、非零任务 ID 和续期规则")
	}
	var renewed *domain.UploadTask
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.LockUserUploadTask(txContext, taskID)
		if err != nil {
			return err
		}
		quota, err := txStore.lockUserUploadQuota(txContext, actor.UserID)
		if err != nil {
			return err
		}
		account := domain.UserProfile{ID: quota.ID, Status: quota.Status, QuotaBytes: quota.QuotaBytes,
			UsedBytes: quota.UsedBytes, ReservedBytes: quota.ReservedBytes}

		// 2. 用数据库时间复算期限，只在当前条件仍匹配时延长。
		var databaseTime time.Time
		clockError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&databaseTime)
		if err := txContext.Err(); err != nil {
			return err
		}
		if clockError != nil {
			return fmt.Errorf("读取分片任务续期数据库时间失败: %w", clockError)
		}
		databaseTime = databaseTime.UTC()
		nextExpiry, err := resolveExpiry(current, &account, expectedExpiry, databaseTime)
		if err != nil {
			return err
		}
		if nextExpiry.Equal(current.ExpiresAt) {
			renewed = current
			return nil
		}
		nextExpiry = nextExpiry.UTC()
		if !nextExpiry.After(current.ExpiresAt) || nextExpiry.Year() > 9999 || nextExpiry.Nanosecond()%1000 != 0 ||
			current.StorageUploadID == nil || !domain.ValidMultipartUploadID(*current.StorageUploadID) {
			return domain.ErrInvalidUploadTaskData
		}
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET expires_at = ?, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? "+
				"AND status = 'UPLOADING' AND upload_mode = 'MULTIPART' AND CAST(storage_upload_id AS BINARY) = ? "+
				"AND reserved_bytes = file_size AND expires_at = ? AND expires_at > UTC_TIMESTAMP(6) "+
				"AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))",
			nextExpiry, actor.UserID, taskID, current.StagedObjectID, current.LeaseVersion, *current.StorageUploadID, expectedExpiry.UTC())
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("写入分片任务续期期限失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskNotReady
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读核对期限及关联字段，等外层提交后再交付。
		updated, err := txStore.LockUserUploadTask(txContext, taskID)
		if err != nil {
			return err
		}
		if !updated.ExpiresAt.Equal(nextExpiry) || updated.UpdatedAt.Before(databaseTime) || !updated.UpdatedAt.Before(current.ExpiresAt) ||
			updated.Status != current.Status || updated.UploadMode != current.UploadMode || updated.StagedObjectID != current.StagedObjectID ||
			updated.LeaseVersion != current.LeaseVersion || !sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) ||
			updated.FileSize != current.FileSize || updated.ReservedBytes != current.ReservedBytes {
			return domain.ErrInvalidUploadTaskData
		}
		renewed = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return renewed, nil
}
