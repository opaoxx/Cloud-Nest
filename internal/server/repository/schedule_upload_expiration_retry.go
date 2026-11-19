package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

const (
	uploadExpirationRetryCode    = "DATABASE_UNAVAILABLE"
	uploadExpirationRetryMessage = "数据服务暂时不可用"
)

// ScheduleUploadExpirationRetry 为单文件过期收尾记录下次尝试时间。
// task 来自已提交的 BeginUploadExpiration，调用方先确认收尾遇到了临时数据库故障。
func (store *Store) ScheduleUploadExpirationRetry(ctx context.Context, task *domain.UploadTask, delaySeconds int) (*domain.UploadTask, error) {
	// 1. 校验任务和等待时间，按所属用户开启事务。
	if store == nil || store.database == nil || ctx == nil || delaySeconds < 1 || delaySeconds > 30 {
		return nil, fmt.Errorf("安排上传过期重试需要数据入口、上下文和 1～30 秒的等待时间")
	}
	if task == nil || task.UserID == 0 || task.ID == 0 || task.StagedObjectID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.UploadMode != "SINGLE" || task.Status != domain.UploadStatusExpiring {
		return nil, domain.ErrUploadTaskNotReady
	}

	var scheduled *domain.UploadTask
	const allowDisabled = true
	err := store.withLockedUserTransaction(ctx, task.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 锁定任务，核对阶段、暂存对象、租约版本和尝试次数。
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.FindUserUploadTask(txContext, task.UserID, task.ID)
		if err != nil {
			return err
		}
		if current.Status != domain.UploadStatusExpiring || current.UploadMode != "SINGLE" {
			return domain.ErrUploadTaskNotReady
		}
		// 尝试次数每次写回只加一，旧快照即使过了等待时间也不能再次安排。
		if current.StagedObjectID != task.StagedObjectID || current.LeaseVersion != task.LeaseVersion ||
			current.AttemptCount != task.AttemptCount || !current.ExpiresAt.Equal(task.ExpiresAt) ||
			current.FileSize != task.FileSize || current.ReservedBytes != task.ReservedBytes {
			return domain.ErrUploadTaskBusy
		}
		if current.AttemptCount == math.MaxUint32 || current.PublishedObjectID != nil ||
			current.ResultEntryID != nil || current.ResolvedName != nil || current.CompletedAt != nil {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 以 MySQL 当前时间计算下次尝试时间，并记录一次临时数据库故障。
		// 任务已经到期，下次收尾可以晚于原 expires_at。
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET next_attempt_at = TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6)), "+
				"attempt_count = attempt_count + 1, last_error_code = ?, last_error_message = ?, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND status = 'EXPIRING' AND upload_mode = 'SINGLE' "+
				"AND staged_object_id = ? AND lease_version = ? AND attempt_count = ? AND expires_at <= UTC_TIMESTAMP(6) "+
				"AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6)) "+
				"AND (next_attempt_at IS NULL OR next_attempt_at <= UTC_TIMESTAMP(6))",
			delaySeconds, uploadExpirationRetryCode, uploadExpirationRetryMessage,
			current.UserID, current.ID, current.StagedObjectID, current.LeaseVersion, current.AttemptCount)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("安排上传过期重试失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 4. 读回核对结果，事务提交后返回任务。
		updated, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		nextAttempt := updated.UpdatedAt.Add(time.Duration(delaySeconds) * time.Second)
		if updated.NextAttemptAt == nil || !updated.NextAttemptAt.Equal(nextAttempt) ||
			updated.UpdatedAt.Before(current.ExpiresAt) || !updated.ExpiresAt.Equal(current.ExpiresAt) ||
			updated.AttemptCount != current.AttemptCount+1 || updated.Status != current.Status || updated.UploadMode != current.UploadMode ||
			updated.StagedObjectID != current.StagedObjectID || updated.LeaseVersion != current.LeaseVersion ||
			updated.FileSize != current.FileSize || updated.ReservedBytes != current.ReservedBytes ||
			updated.PublishedObjectID != nil || updated.ResultEntryID != nil || updated.ResolvedName != nil || updated.CompletedAt != nil ||
			updated.LastErrorCode == nil || *updated.LastErrorCode != uploadExpirationRetryCode ||
			updated.LastErrorMessage == nil || *updated.LastErrorMessage != uploadExpirationRetryMessage {
			return domain.ErrInvalidUploadTaskData
		}
		scheduled = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return scheduled, nil
}
