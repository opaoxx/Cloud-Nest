package repository

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// BeginUploadExpiration 按到期候选复核并推进清理阶段，提交后返回任务。
// 针对可继续处理的 EXPIRING 候选，返回当前任务供后续收尾使用。
func (store *Store) BeginUploadExpiration(ctx context.Context, candidate domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
	// 1. 校验候选，开启所属用户的过期处理事务。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("开始上传过期处理需要数据入口和上下文")
	}
	expiry := candidate.ExpiresAt.UTC()
	if candidate.TaskID == 0 || candidate.UserID == 0 || candidate.StagedObjectID == 0 ||
		!slices.Contains(uploadExpirationStatuses[:], candidate.Status) ||
		(candidate.UploadMode != "" && candidate.UploadMode != "SINGLE" && candidate.UploadMode != "MULTIPART") ||
		expiry.Year() < 1000 || expiry.Year() > 9999 || expiry.Nanosecond()%1000 != 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	var expiring *domain.UploadTask
	const allowDisabled = true
	err := store.withLockedUserTransaction(ctx, candidate.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 复核当前任务和数据库时间，判断是否仍可处理。
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.FindUserUploadTask(txContext, candidate.UserID, candidate.TaskID)
		if err != nil {
			return err
		}
		if !slices.Contains(uploadExpirationStatuses[:], current.Status) {
			return domain.ErrUploadTaskNotReady
		}
		if current.Status != candidate.Status || current.StagedObjectID != candidate.StagedObjectID ||
			current.LeaseVersion != candidate.LeaseVersion || !current.ExpiresAt.Equal(expiry) ||
			(candidate.UploadMode != "" && current.UploadMode != candidate.UploadMode) {
			return domain.ErrUploadTaskBusy
		}
		if current.ResultEntryID != nil || current.PublishedObjectID != nil || current.ResolvedName != nil || current.CompletedAt != nil {
			return domain.ErrInvalidUploadTaskData
		}
		var now time.Time
		clockError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
		if err := txContext.Err(); err != nil {
			return err
		}
		if clockError != nil {
			return fmt.Errorf("读取上传过期处理时间失败: %w", clockError)
		}
		if now.Before(current.ExpiresAt) || (current.Status == domain.UploadStatusExpiring &&
			current.NextAttemptAt != nil && now.Before(*current.NextAttemptAt)) {
			return domain.ErrUploadTaskNotReady
		}
		if current.LeaseUntil != nil && now.Before(*current.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		if current.Status == domain.UploadStatusExpiring {
			expiring = current
			return nil
		}

		// 3. 条件写入 EXPIRING，读回并提交结果。
		// 上传阶段结束后清掉之前的租约和上传重试时间，终结时再释放预占。
		result := txStore.database.WithContext(txContext).Exec(
			"UPDATE upload_tasks SET status = 'EXPIRING', lease_owner = NULL, lease_until = NULL, "+
				"next_attempt_at = NULL, updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND staged_object_id = ? AND lease_version = ? AND status = ? "+
				"AND upload_mode = ? AND expires_at = ? AND expires_at <= UTC_TIMESTAMP(6) "+
				"AND (lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))",
			current.UserID, current.ID, current.StagedObjectID, current.LeaseVersion, current.Status, current.UploadMode, current.ExpiresAt)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("开始上传过期处理失败: %w", result.Error)
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
		if updated.Status != domain.UploadStatusExpiring || updated.StagedObjectID != current.StagedObjectID || updated.UploadMode != current.UploadMode ||
			updated.LeaseOwner != nil || updated.LeaseUntil != nil || updated.LeaseVersion != current.LeaseVersion || updated.NextAttemptAt != nil ||
			updated.ReservedBytes != current.ReservedBytes || updated.FileSize != current.FileSize || !updated.ExpiresAt.Equal(expiry) ||
			updated.UpdatedAt.Before(now) || updated.ResultEntryID != nil || updated.PublishedObjectID != nil ||
			updated.ResolvedName != nil || updated.CompletedAt != nil {
			return domain.ErrInvalidUploadTaskData
		}
		expiring = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return expiring, nil
}
