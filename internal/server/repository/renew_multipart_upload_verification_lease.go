package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// RenewMultipartUploadVerificationLease 延长已领取的分片校验租约，提交后返回任务。
// 同一持有者可重复使用原领取快照，租约版本和原分片声明保持原值。
func (store *Store) RenewMultipartUploadVerificationLease(ctx context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 开启短事务，加锁复核任务、完整预占和原分片会话。
	if store == nil || store.database == nil || ctx == nil || claimed == nil || claimed.UserID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	var renewed *domain.UploadTask
	err := store.withActiveUserTransaction(ctx, claimed.UserID, func(txContext context.Context, txStore *Store) error {
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.findVerifyingUploadWithFullReservation(txContext, claimed, "MULTIPART")
		if err != nil {
			return err
		}

		// 2. 用数据库时间检查两种期限，现有期限更长或相等时直接复用。
		var now time.Time
		clockError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
		if err := txContext.Err(); err != nil {
			return err
		}
		if clockError != nil {
			return fmt.Errorf("读取分片校验续租数据库时间失败: %w", clockError)
		}
		now = now.UTC()
		nextDeadline := now.Add(time.Minute)
		if now.Year() < 1000 || nextDeadline.Year() > 9999 || now.Nanosecond()%1000 != 0 {
			return domain.ErrInvalidUploadTaskData
		}
		if !now.Before(current.ExpiresAt) {
			return domain.ErrUploadTaskNotReady
		}
		if !now.Before(*current.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		if !nextDeadline.After(*current.LeaseUntil) {
			renewed = current
			return nil
		}

		// 3. 在同一租约下只延长期限，读回核对后交由外层提交。
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE upload_tasks
			SET lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND), updated_at = UTC_TIMESTAMP(6)
			WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND CAST(lease_owner AS BINARY) = ?
			  AND status = 'VERIFYING' AND upload_mode = 'MULTIPART' AND CAST(storage_upload_id AS BINARY) = ?
			  AND reserved_bytes = file_size AND expires_at = ? AND expires_at > UTC_TIMESTAMP(6)
			  AND lease_until = ? AND lease_until > UTC_TIMESTAMP(6)
			  AND lease_until < DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND)`,
			current.UserID, current.ID, current.StagedObjectID, current.LeaseVersion, *current.LeaseOwner,
			*current.StorageUploadID, current.ExpiresAt, *current.LeaseUntil)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("续租分片校验失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}
		updated, err := lockedStore.FindUserUploadTask(txContext, current.UserID, current.ID)
		if err != nil {
			return err
		}
		if updated.Status != current.Status || updated.UploadMode != current.UploadMode || updated.StagedObjectID != current.StagedObjectID ||
			updated.LeaseVersion != current.LeaseVersion || !sameUploadStorageText(updated.LeaseOwner, current.LeaseOwner) ||
			updated.LeaseUntil == nil || !updated.LeaseUntil.After(*current.LeaseUntil) || !updated.LeaseUntil.Equal(updated.UpdatedAt.Add(time.Minute)) ||
			updated.UpdatedAt.Before(now) || updated.UpdatedAt.Before(current.UpdatedAt) ||
			!updated.UpdatedAt.Before(*current.LeaseUntil) || !updated.UpdatedAt.Before(current.ExpiresAt) ||
			!sameUploadStorageText(updated.StorageUploadID, current.StorageUploadID) || !sameMultipartUploadDeclaration(current, updated) {
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
