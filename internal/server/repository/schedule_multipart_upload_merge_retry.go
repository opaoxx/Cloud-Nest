package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// ScheduleMultipartUploadMergeRetry 为当前合并租约登记下次恢复核对。
// 调用方先确认仍需恢复，并指定 1～30 秒等待。新一轮最早在原租约截止后进入候选。
func (store *Store) ScheduleMultipartUploadMergeRetry(ctx context.Context, claimed *domain.UploadTask, delaySeconds int) (*domain.UploadTask, error) {
	// 1. 校验输入，按账号加锁复核任务和当前合并租约。
	if store == nil || store.database == nil || ctx == nil || delaySeconds < 1 || delaySeconds > 30 {
		return nil, fmt.Errorf("安排分片合并重试需要数据入口、上下文和 1～30 秒的等待时间")
	}
	if !validMultipartMergeClaim(claimed) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	const retryCode, retryMessage = "MERGE_RECHECK_REQUIRED", "分片合并结果需要重新核对"
	var scheduled *domain.UploadTask
	err := store.withActiveUserTransaction(ctx, claimed.UserID, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.lockMultipartMergeTask(txContext, claimed)
		if err != nil {
			return err
		}
		if !current.LeaseUntil.Equal(*claimed.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		if current.LeaseVersion == math.MaxUint64 {
			return domain.ErrUploadLeaseVersionExhausted
		}
		if current.AttemptCount == math.MaxUint32 {
			return domain.ErrInvalidUploadTaskData
		}

		// 2. 用数据库时间登记下次核对，递增次数并释放租约。
		// 原租约截止为核对窗口的下限，任务到期后交由过期流程。
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE upload_tasks
			SET next_attempt_at = LEAST(expires_at, GREATEST(lease_until, TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6)))),
			    attempt_count = attempt_count + 1, last_error_code = ?, last_error_message = ?,
			    lease_owner = NULL, lease_until = NULL, updated_at = UTC_TIMESTAMP(6)
			WHERE user_id = ? AND id = ? AND staged_object_id = ? AND status = 'COMPLETING' AND upload_mode = 'MULTIPART'
			  AND CAST(storage_upload_id AS BINARY) = ? AND CAST(lease_owner AS BINARY) = ?
			  AND lease_version = ? AND lease_until = ? AND attempt_count = ?
			  AND lease_until > UTC_TIMESTAMP(6) AND expires_at > UTC_TIMESTAMP(6) AND reserved_bytes = file_size`,
			delaySeconds, retryCode, retryMessage, current.UserID, current.ID, current.StagedObjectID,
			*current.StorageUploadID, *current.LeaseOwner, current.LeaseVersion, *current.LeaseUntil, current.AttemptCount)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("安排分片合并重试失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读核对时间和任务字段，事务提交后返回。
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		updated, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		nextAttempt := updated.UpdatedAt.Add(time.Duration(delaySeconds) * time.Second)
		if nextAttempt.Before(*current.LeaseUntil) {
			nextAttempt = *current.LeaseUntil
		}
		if current.ExpiresAt.Before(nextAttempt) {
			nextAttempt = current.ExpiresAt
		}
		if updated.NextAttemptAt == nil || !updated.NextAttemptAt.Equal(nextAttempt) || updated.UpdatedAt.Before(current.UpdatedAt) ||
			!updated.UpdatedAt.Before(*current.LeaseUntil) || !updated.UpdatedAt.Before(current.ExpiresAt) ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.LeaseVersion != current.LeaseVersion ||
			updated.AttemptCount != current.AttemptCount+1 || updated.Status != current.Status || updated.UploadMode != current.UploadMode ||
			updated.StagedObjectID != current.StagedObjectID || !sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) ||
			!sameMultipartUploadDeclaration(current, updated) || updated.LastErrorCode == nil || *updated.LastErrorCode != retryCode ||
			updated.LastErrorMessage == nil || *updated.LastErrorMessage != retryMessage {
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
