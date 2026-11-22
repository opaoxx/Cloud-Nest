package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// ScheduleMultipartUploadCancellationRetry 按当前取消收尾租约登记下次尝试。
// 接受 1～30 秒的等待，候选最早在原租约截止后重新可见。
func (store *Store) ScheduleMultipartUploadCancellationRetry(
	ctx context.Context, claimed *domain.UploadTask, delaySeconds int, reason domain.MultipartUploadCancellationRetryReason,
) (*domain.UploadTask, error) {
	return store.scheduleMultipartUploadCleanupRetry(ctx, claimed, delaySeconds, reason, domain.UploadStatusCancelling)
}

// ScheduleMultipartUploadExpirationRetry 按当前到期收尾租约登记下次尝试。
// 接受 1～30 秒的等待，保留原任务的全额预占，候选最早在原租约截止后重新可见。
func (store *Store) ScheduleMultipartUploadExpirationRetry(
	ctx context.Context, claimed *domain.UploadTask, delaySeconds int, reason domain.MultipartUploadCleanupRetryReason,
) (*domain.UploadTask, error) {
	return store.scheduleMultipartUploadCleanupRetry(ctx, claimed, delaySeconds, reason, domain.UploadStatusExpiring)
}

func (store *Store) scheduleMultipartUploadCleanupRetry(
	ctx context.Context, claimed *domain.UploadTask, delaySeconds int, reason domain.MultipartUploadCleanupRetryReason,
	pendingStatus domain.UploadStatus,
) (*domain.UploadTask, error) {
	// 1. 校验重试参数，在账号事务内复核任务和当前租约。
	if store == nil || store.database == nil || ctx == nil || delaySeconds < 1 || delaySeconds > 30 {
		return nil, fmt.Errorf("安排分片收尾重试需要数据入口、上下文和 1～30 秒等待时间")
	}
	if !validMultipartUploadCleanupClaim(claimed, pendingStatus) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	var message string
	switch reason {
	case domain.MultipartCleanupRetryStorage:
		message = domain.ErrUploadStorageUnavailable.Error()
	case domain.MultipartCleanupRetryDatabase:
		message = uploadCleanupRetryMessage
	default:
		return nil, domain.ErrInvalidUploadTaskData
	}
	var scheduled *domain.UploadTask
	const allowDisabled = true
	err := store.withLockedUserTransaction(ctx, claimed.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.readMultipartUploadCleanupLease(txContext, claimed, pendingStatus)
		if err != nil {
			return err
		}
		if current.AttemptCount != claimed.AttemptCount {
			return domain.ErrUploadTaskBusy
		}
		if current.LeaseVersion == math.MaxUint64 {
			return domain.ErrUploadLeaseVersionExhausted
		}
		if current.AttemptCount == math.MaxUint32 {
			return domain.ErrInvalidUploadTaskData
		}

		// 2. 取原租约截止与退避截止中的较晚时间，登记重试并释放本次租约。
		// 用写入语句的时间复查租约，并要求到期任务已达到原期限。
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE upload_tasks
			SET next_attempt_at = GREATEST(lease_until, TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6))),
			    attempt_count = attempt_count + 1, last_error_code = ?, last_error_message = ?,
			    lease_owner = NULL, lease_until = NULL, updated_at = UTC_TIMESTAMP(6)
			WHERE user_id = ? AND id = ? AND staged_object_id = ? AND status = ? AND upload_mode = 'MULTIPART'
			  AND expires_at = ? AND (status = 'CANCELLING' OR expires_at <= UTC_TIMESTAMP(6))
			  AND CAST(storage_upload_id AS BINARY) <=> ? AND CAST(lease_owner AS BINARY) = ?
			  AND lease_version = ? AND lease_until = ? AND lease_until > UTC_TIMESTAMP(6) AND attempt_count = ?
			  AND (next_attempt_at IS NULL OR next_attempt_at <= UTC_TIMESTAMP(6))
			  AND reserved_bytes = file_size`,
			delaySeconds, string(reason), message, current.UserID, current.ID, current.StagedObjectID, pendingStatus, current.ExpiresAt,
			current.StorageUploadID, *current.LeaseOwner, current.LeaseVersion, *current.LeaseUntil, current.AttemptCount)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("安排分片收尾重试失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 读回重试安排，核对原会话和预占，提交后返回。
		updated, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		nextAttempt := updated.UpdatedAt.Add(time.Duration(delaySeconds) * time.Second)
		if nextAttempt.Before(*current.LeaseUntil) {
			nextAttempt = *current.LeaseUntil
		}
		if updated.NextAttemptAt == nil || !updated.NextAttemptAt.Equal(nextAttempt) ||
			updated.UpdatedAt.Before(current.UpdatedAt) || !updated.UpdatedAt.Before(*current.LeaseUntil) ||
			(pendingStatus == domain.UploadStatusExpiring && updated.UpdatedAt.Before(current.ExpiresAt)) ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.LeaseVersion != current.LeaseVersion ||
			updated.AttemptCount != current.AttemptCount+1 || updated.Status != current.Status || updated.UploadMode != current.UploadMode ||
			updated.StagedObjectID != current.StagedObjectID || !sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) ||
			!sameMultipartUploadDeclaration(current, updated) || updated.LastErrorCode == nil || *updated.LastErrorCode != string(reason) ||
			updated.LastErrorMessage == nil || *updated.LastErrorMessage != message {
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
