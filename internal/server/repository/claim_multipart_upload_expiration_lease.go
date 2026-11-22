package repository

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ClaimMultipartUploadExpirationLease 推进或续接分片过期处理，并领取六十秒收尾租约。
// 只有账号事务提交后才返回任务，供调用方在事务外处理存储。
func (store *Store) ClaimMultipartUploadExpirationLease(
	ctx context.Context, candidate domain.UploadExpirationCandidate, leaseOwner string,
) (*domain.UploadTask, error) {
	// 1. 校验持有人，在账号事务内复核候选和暂存对象。
	if len(leaseOwner) == 0 || len(leaseOwner) > 64 || strings.ContainsFunc(leaseOwner, func(character rune) bool { return character > 127 }) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	var claimed *domain.UploadTask
	err := store.WithMultipartUploadExpirationTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, current *domain.UploadTask, object *domain.StoredObject) error {
		if current.LeaseVersion == math.MaxUint64 {
			return domain.ErrUploadLeaseVersionExhausted
		}

		// 2. 条件写入 EXPIRING 和新租约，首次推进时清掉原上传阶段的重试时间。
		nextAttemptAt := current.NextAttemptAt
		if current.Status != domain.UploadStatusExpiring {
			nextAttemptAt = nil
		}
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE upload_tasks
			SET status = 'EXPIRING', next_attempt_at = ?, lease_owner = ?,
			    lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND),
			    lease_version = lease_version + 1, updated_at = UTC_TIMESTAMP(6)
			WHERE user_id = ? AND id = ? AND staged_object_id = ?
			  AND lease_version = ? AND lease_version < ?
			  AND status = ? AND upload_mode = 'MULTIPART' AND reserved_bytes = file_size
			  AND CAST(storage_upload_id AS BINARY) <=> ?
			  AND expires_at = ? AND expires_at <= UTC_TIMESTAMP(6)
			  AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))
			  AND (status <> 'EXPIRING' OR next_attempt_at IS NULL OR next_attempt_at <= UTC_TIMESTAMP(6))`,
			nextAttemptAt, leaseOwner, current.UserID, current.ID, object.ID, current.LeaseVersion, uint64(math.MaxUint64),
			current.Status, current.StorageUploadID, current.ExpiresAt)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("领取分片过期收尾租约失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 读回复核新租约、原会话、分片声明和预占，提交后才向调用方交付。
		updated, err := txStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		retryTimeMatches := updated.NextAttemptAt == nil && nextAttemptAt == nil ||
			updated.NextAttemptAt != nil && nextAttemptAt != nil && updated.NextAttemptAt.Equal(*nextAttemptAt)
		if updated.Status != domain.UploadStatusExpiring || updated.UploadMode != "MULTIPART" || updated.StagedObjectID != object.ID ||
			updated.LeaseVersion != current.LeaseVersion+1 || updated.LeaseOwner == nil || *updated.LeaseOwner != leaseOwner ||
			updated.LeaseUntil == nil || !updated.LeaseUntil.Equal(updated.UpdatedAt.Add(time.Minute)) || updated.UpdatedAt.Before(current.UpdatedAt) ||
			!retryTimeMatches || updated.AttemptCount != current.AttemptCount ||
			!sameUploadStorageText(updated.LastErrorCode, current.LastErrorCode) || !sameUploadStorageText(updated.LastErrorMessage, current.LastErrorMessage) ||
			!sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) || !sameMultipartUploadDeclaration(current, updated) {
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
