package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// ClaimUploadVerificationLease 为内部 worker 领取校验租约，提交后返回任务。
// candidate 来自数据库扫描。持有者由 worker 生成，调用须使用事务外 Store。
func (store *Store) ClaimUploadVerificationLease(ctx context.Context, candidate domain.UploadVerificationCandidate, leaseOwner string) (*domain.UploadTask, error) {
	return store.claimUploadLease(ctx, uploadLeaseCandidate(candidate), leaseOwner, domain.UploadStatusVerifying)
}

// ClaimMultipartUploadInitializationLease 为尚未绑定存储会话的任务领取初始化租约。
// 提交后返回任务及新版本，调用方再执行事务外的存储初始化。
func (store *Store) ClaimMultipartUploadInitializationLease(ctx context.Context, candidate domain.MultipartUploadInitializationCandidate, leaseOwner string) (*domain.UploadTask, error) {
	return store.claimUploadLease(ctx, uploadLeaseCandidate(candidate), leaseOwner, domain.UploadStatusCreating)
}

// ClaimMultipartUploadCompletionLease 为已保存完成意图的任务领取后台合并租约。
// 返回提交后的任务及新版本，后续才能读取冻结回执并执行事务外的存储操作。
func (store *Store) ClaimMultipartUploadCompletionLease(ctx context.Context, candidate domain.MultipartUploadCompletionCandidate, leaseOwner string) (*domain.UploadTask, error) {
	return store.claimUploadLease(ctx, uploadLeaseCandidate(candidate), leaseOwner, domain.UploadStatusCompleting)
}

type uploadLeaseCandidate struct {
	TaskID, UserID, StagedObjectID, LeaseVersion uint64
}

func (store *Store) claimUploadLease(ctx context.Context, candidate uploadLeaseCandidate, leaseOwner string, status domain.UploadStatus) (*domain.UploadTask, error) {
	// 1. 校验候选与持有者，开启所属用户的短事务。
	var action, clockAction, mode, taskCondition string
	var objectStatus domain.StoredObjectStatus
	switch status {
	case domain.UploadStatusVerifying:
		action, clockAction = "上传校验", "校验"
		mode, objectStatus = "SINGLE", domain.StoredObjectStatusVerifying
		taskCondition = "status = 'VERIFYING' AND upload_mode = 'SINGLE'"
	case domain.UploadStatusCreating:
		action, clockAction = "分片初始化", "分片初始化"
		mode, objectStatus = "MULTIPART", domain.StoredObjectStatusStaging
		taskCondition = "status = 'CREATING' AND upload_mode = 'MULTIPART' AND storage_upload_id IS NULL"
	case domain.UploadStatusCompleting:
		action, clockAction = "分片合并", "分片合并"
		mode, objectStatus = "MULTIPART", domain.StoredObjectStatusStaging
		taskCondition = "status = 'COMPLETING' AND upload_mode = 'MULTIPART' AND CAST(storage_upload_id AS BINARY) = ?"
	default:
		return nil, domain.ErrInvalidUploadTaskData
	}
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("领取%s租约需要数据入口和上下文", action)
	}
	if candidate.UserID == 0 || candidate.TaskID == 0 || candidate.StagedObjectID == 0 || len(leaseOwner) == 0 || len(leaseOwner) > 64 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	for _, character := range leaseOwner {
		if character > 127 {
			return nil, domain.ErrInvalidUploadTaskData
		}
	}

	var claimed *domain.UploadTask
	err := store.withActiveUserTransaction(ctx, candidate.UserID, func(txContext context.Context, txStore *Store) error {
		// 2. 加锁复查任务、分片会话、暂存对象和可领取时间。
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		task, err := lockedStore.FindUserUploadTask(txContext, candidate.UserID, candidate.TaskID)
		if err != nil {
			return err
		}
		if task.Status != status || task.UploadMode != mode ||
			(status == domain.UploadStatusCreating && task.StorageUploadID != nil) ||
			(status == domain.UploadStatusCompleting && task.StorageUploadID == nil) {
			return domain.ErrUploadTaskNotReady
		}
		if mode == "MULTIPART" {
			if task.ExpectedSHA256 == nil || task.PartSize == nil || task.PartCount == nil ||
				!domain.ValidMultipartUploadLayout(task.FileSize, *task.PartSize, *task.PartCount) {
				return domain.ErrInvalidUploadTaskData
			}
			if status == domain.UploadStatusCompleting && !domain.ValidMultipartUploadID(*task.StorageUploadID) {
				return domain.ErrInvalidUploadTaskData
			}
		} else if task.FileSize > domain.MaxSingleUploadSizeBytes {
			return domain.ErrUploadFileTooLarge
		}
		if task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil || task.ResolvedName != nil ||
			task.ResultEntryID != nil || task.CompletedAt != nil || (task.LeaseOwner != nil && (*task.LeaseOwner == "" || task.LeaseVersion == 0)) {
			return domain.ErrInvalidUploadTaskData
		}
		if task.StagedObjectID != candidate.StagedObjectID || task.LeaseVersion != candidate.LeaseVersion {
			return domain.ErrUploadTaskBusy
		}
		if task.LeaseVersion == math.MaxUint64 {
			return domain.ErrUploadLeaseVersionExhausted
		}
		var now time.Time
		queryError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
		if err := txContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return fmt.Errorf("读取%s租约数据库时间失败: %w", clockAction, queryError)
		}
		now = now.UTC()
		if now.Year() < 1000 || now.Add(time.Minute).Year() > 9999 || now.Nanosecond()%1000 != 0 {
			return domain.ErrInvalidUploadTaskData
		}
		if !now.Before(task.ExpiresAt) || (task.NextAttemptAt != nil && now.Before(*task.NextAttemptAt)) {
			return domain.ErrUploadTaskNotReady
		}
		if task.LeaseUntil != nil && now.Before(*task.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		object, err := lockedStore.FindUserStoredObject(txContext, candidate.UserID, candidate.StagedObjectID)
		if err != nil {
			return err
		}
		if object.Status != objectStatus || object.FileSize != task.FileSize ||
			object.VerifiedAt != nil || object.VerifiedSHA256 != nil || object.UnreferencedAt != nil {
			return domain.ErrInvalidStoredObjectData
		}
		if mode == "MULTIPART" && (object.StorageVersionID != nil || object.StorageETag != nil || object.GCAfter != nil || object.LastErrorCode != nil) {
			return domain.ErrInvalidStoredObjectData
		}

		// 3. 条件写入 60 秒租约并递增版本。
		arguments := []any{leaseOwner, candidate.UserID, candidate.TaskID, candidate.StagedObjectID, candidate.LeaseVersion, uint64(math.MaxUint64)}
		if status == domain.UploadStatusCompleting {
			arguments = append(arguments, *task.StorageUploadID)
		}
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET lease_owner = ?, lease_until = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 60 SECOND), "+
				"lease_version = lease_version + 1, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND lease_version < ? "+
				"AND "+taskCondition+" AND reserved_bytes = file_size AND expires_at > UTC_TIMESTAMP(6) "+
				"AND (next_attempt_at IS NULL OR next_attempt_at <= UTC_TIMESTAMP(6)) "+
				"AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))",
			arguments...)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("领取%s租约失败: %w", action, result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrUploadTaskBusy
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidUploadTaskData
		}

		// 4. 回读确认新租约、分片声明和会话原值，提交后返回任务。
		updated, err := lockedStore.FindUserUploadTask(txContext, candidate.UserID, candidate.TaskID)
		if errors.Is(err, domain.ErrUploadTaskNotFound) {
			return domain.ErrInvalidUploadTaskData
		}
		if err != nil {
			return err
		}
		if updated.Status != task.Status || updated.UploadMode != task.UploadMode || updated.StagedObjectID != candidate.StagedObjectID ||
			updated.LeaseVersion != candidate.LeaseVersion+1 || updated.LeaseOwner == nil || *updated.LeaseOwner != leaseOwner ||
			updated.LeaseUntil == nil || !updated.LeaseUntil.Equal(updated.UpdatedAt.Add(time.Minute)) ||
			updated.UpdatedAt.Before(now) || !updated.UpdatedAt.Before(task.ExpiresAt) ||
			updated.FileSize != task.FileSize || updated.ReservedBytes != task.ReservedBytes || !updated.ExpiresAt.Equal(task.ExpiresAt) {
			return domain.ErrInvalidUploadTaskData
		}
		if mode == "MULTIPART" && (!sameUploadStorageText(updated.StorageUploadID, task.StorageUploadID) || !sameMultipartUploadDeclaration(task, updated)) {
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
