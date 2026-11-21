package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// ScheduleMultipartUploadInitializationRetry 为当前初始化租约记录一次存储重试。
// claimed 来自已提交的领租操作，调用方先确认存储失败可重试，再指定 1～30 秒等待。
func (store *Store) ScheduleMultipartUploadInitializationRetry(ctx context.Context, claimed *domain.UploadTask, delaySeconds int) (*domain.UploadTask, error) {
	// 1. 校验输入，在所属账号事务中加锁复核当前任务及租约。
	if store == nil || store.database == nil || ctx == nil || delaySeconds < 1 || delaySeconds > 30 {
		return nil, fmt.Errorf("安排分片初始化重试需要数据入口、上下文和 1～30 秒的等待时间")
	}
	if !validMultipartInitializationClaim(claimed) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	const retryCode = "STORAGE_UNAVAILABLE"
	retryMessage := domain.ErrUploadStorageUnavailable.Error()
	var scheduled *domain.UploadTask
	err := store.withActiveUserTransaction(ctx, claimed.UserID, func(txContext context.Context, txStore *Store) error {
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.findCreatingMultipartWithLease(txContext, claimed)
		if err != nil {
			return err
		}
		if current.LeaseVersion == math.MaxUint64 {
			return domain.ErrUploadLeaseVersionExhausted
		}
		if current.AttemptCount == math.MaxUint32 {
			return domain.ErrInvalidUploadTaskData
		}

		// 2. 以 MySQL UTC 时间登记重试，释放租约，等待时间最多延至任务到期。
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET next_attempt_at = LEAST(expires_at, TIMESTAMPADD(SECOND, ?, UTC_TIMESTAMP(6))), "+
				"attempt_count = attempt_count + 1, last_error_code = ?, last_error_message = ?, "+
				"lease_owner = NULL, lease_until = NULL, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND CAST(lease_owner AS BINARY) = ? "+
				"AND status = 'CREATING' AND upload_mode = 'MULTIPART' AND storage_upload_id IS NULL AND reserved_bytes = file_size "+
				"AND lease_until > UTC_TIMESTAMP(6) AND expires_at > UTC_TIMESTAMP(6)",
			delaySeconds, retryCode, retryMessage, current.UserID, current.ID, current.StagedObjectID, current.LeaseVersion, *current.LeaseOwner)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("安排分片初始化重试失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 回读校验等待时间及保留字段，事务提交后交付任务。
		updated, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		nextAttempt := updated.UpdatedAt.Add(time.Duration(delaySeconds) * time.Second)
		if current.ExpiresAt.Before(nextAttempt) {
			nextAttempt = current.ExpiresAt
		}
		if updated.NextAttemptAt == nil || !updated.NextAttemptAt.Equal(nextAttempt) ||
			!updated.UpdatedAt.Before(*current.LeaseUntil) || !updated.UpdatedAt.Before(current.ExpiresAt) ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.LeaseVersion != current.LeaseVersion ||
			updated.AttemptCount != current.AttemptCount+1 || updated.Status != current.Status || updated.UploadMode != current.UploadMode ||
			updated.StagedObjectID != current.StagedObjectID || updated.StorageUploadID != nil ||
			!sameMultipartUploadDeclaration(current, updated) ||
			updated.LastErrorCode == nil || *updated.LastErrorCode != retryCode ||
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
