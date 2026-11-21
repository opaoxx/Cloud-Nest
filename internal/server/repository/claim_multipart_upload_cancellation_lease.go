package repository

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ClaimMultipartUploadCancellationLease 为已受理的分片取消领取六十秒收尾租约。
// 提交后返回任务，调用方再执行事务外的存储收尾。
func (store *Store) ClaimMultipartUploadCancellationLease(
	ctx context.Context, candidate domain.UploadCancellationCandidate, leaseOwner string,
) (*domain.UploadTask, error) {
	// 1. 检查持有人标识，沿用账号、任务和暂存对象的锁内复核。
	if len(leaseOwner) == 0 || len(leaseOwner) > 64 || strings.ContainsFunc(leaseOwner, func(character rune) bool { return character > 127 }) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	var claimed *domain.UploadTask
	err := store.WithMultipartUploadCancellationTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, current *domain.UploadTask, object *domain.StoredObject) error {
		if current.LeaseVersion == math.MaxUint64 {
			return domain.ErrUploadLeaseVersionExhausted
		}

		// 2. 在原锁内条件领取租约。已受理的取消可以在原任务期限后继续。
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE upload_tasks
			SET lease_owner = ?, lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND),
			    lease_version = lease_version + 1, updated_at = UTC_TIMESTAMP(6)
			WHERE user_id = ? AND id = ? AND staged_object_id = ?
			  AND lease_version = ? AND lease_version < ?
			  AND status = 'CANCELLING' AND upload_mode = 'MULTIPART' AND reserved_bytes = file_size
			  AND CAST(storage_upload_id AS BINARY) <=> ?
			  AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))
			  AND (next_attempt_at IS NULL OR next_attempt_at <= UTC_TIMESTAMP(6))`,
			leaseOwner, current.UserID, current.ID, object.ID, current.LeaseVersion, uint64(math.MaxUint64), current.StorageUploadID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("领取分片取消收尾租约失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 3. 读回复核新租约和原声明，整个事务提交后再交付。
		updated, err := txStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		if updated.Status != current.Status || updated.UploadMode != current.UploadMode || updated.StagedObjectID != object.ID ||
			updated.LeaseVersion != current.LeaseVersion+1 || updated.LeaseOwner == nil || *updated.LeaseOwner != leaseOwner ||
			updated.LeaseUntil == nil || !updated.LeaseUntil.Equal(updated.UpdatedAt.Add(time.Minute)) || updated.UpdatedAt.Before(current.UpdatedAt) ||
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
