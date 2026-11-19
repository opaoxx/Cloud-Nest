package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BeginUserUploadCancellation 受理单文件上传取消，提交后返回当前任务。
// 已进入 CANCELLING 或终态时复用当前记录。actor 须来自认证流程。
func (store *Store) BeginUserUploadCancellation(ctx context.Context, actor domain.Actor, taskID uint64) (*domain.UploadTask, error) {
	// 1. 进入已认证用户事务，锁定当前上传任务。
	if store == nil || store.database == nil || ctx == nil || taskID == 0 {
		return nil, fmt.Errorf("受理上传取消需要数据入口、上下文和非零任务 ID")
	}
	var resultTask *domain.UploadTask
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.LockUserUploadTask(txContext, taskID)
		if err != nil {
			return err
		}

		// 2. 读取数据库时间，判断是否需要写入 CANCELLING。
		var databaseTime time.Time
		clockError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&databaseTime)
		if err := txContext.Err(); err != nil {
			return err
		}
		if clockError != nil {
			return fmt.Errorf("读取上传取消处理时间失败: %w", clockError)
		}
		shouldBegin, err := domain.ShouldBeginUploadCancellation(current, databaseTime)
		if err != nil {
			return err
		}
		if !shouldBegin {
			resultTask = current
			return nil
		}
		if current.ResultEntryID != nil || current.PublishedObjectID != nil || current.ResolvedName != nil || current.CompletedAt != nil {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 按当前状态、期限和租约做条件更新。
		// 取消收尾再结算预占。原上传的租约和等待时间在本次受理时清理。
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET status = 'CANCELLING', lease_owner = NULL, lease_until = NULL, "+
				"next_attempt_at = NULL, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND status = ? "+
				"AND upload_mode = 'SINGLE' AND expires_at = ? AND expires_at > UTC_TIMESTAMP(6) "+
				"AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))",
			current.UserID, current.ID, current.StagedObjectID, current.LeaseVersion, current.Status, current.ExpiresAt)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("写入上传取消状态失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskNotReady
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 4. 读回复核，事务提交后返回任务。
		updated, err := txStore.LockUserUploadTask(txContext, taskID)
		if err != nil {
			return err
		}
		if updated.Status != domain.UploadStatusCancelling || updated.UploadMode != "SINGLE" ||
			updated.StagedObjectID != current.StagedObjectID || updated.LeaseVersion != current.LeaseVersion ||
			updated.ReservedBytes != current.ReservedBytes || updated.FileSize != current.FileSize ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.NextAttemptAt != nil ||
			updated.ResultEntryID != nil || updated.PublishedObjectID != nil || updated.ResolvedName != nil || updated.CompletedAt != nil ||
			!updated.ExpiresAt.Equal(current.ExpiresAt) || updated.UpdatedAt.Before(databaseTime) || !updated.UpdatedAt.Before(current.ExpiresAt) {
			return domain.ErrInvalidUploadTaskData
		}
		resultTask = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resultTask, nil
}
