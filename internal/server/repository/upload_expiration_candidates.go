package repository

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

const maxUploadExpirationCandidates = 100

// ListUploadExpirationCandidates 在事务外读取已到期、可尝试处理的任务。
// 后续写入须在所属用户事务中重新核对当前任务和租约。
func (store *Store) ListUploadExpirationCandidates(ctx context.Context, limit int) ([]domain.UploadExpirationCandidate, error) {
	// 1. 校验读取入口和批量上限。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > maxUploadExpirationCandidates {
		return nil, fmt.Errorf("扫描到期上传需要数据入口、上下文和 1～100 条的批量大小")
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描到期上传须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 按 MySQL 时间筛选可处理的到期任务。
	statuses := []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting,
		domain.UploadStatusVerifying, domain.UploadStatusExpiring,
	}
	candidates := make([]domain.UploadExpirationCandidate, 0, limit)
	queryError := store.database.WithContext(queryContext).Table("upload_tasks").
		Select("id AS task_id", "user_id", "staged_object_id", "lease_version", "status", "expires_at").
		Where("status IN ? AND expires_at <= UTC_TIMESTAMP(6)", statuses).
		Where("(lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))").
		Where("(status <> ? OR next_attempt_at IS NULL OR next_attempt_at <= UTC_TIMESTAMP(6))", domain.UploadStatusExpiring).
		Order("expires_at ASC, id ASC").Limit(limit).Find(&candidates).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描到期上传失败: %w", queryError)
	}

	// 3. 核对候选范围和顺序，返回给后续事务复核。
	if len(candidates) > limit {
		return nil, domain.ErrInvalidUploadTaskData
	}
	var previousExpiry time.Time
	var previousTaskID uint64
	for index, candidate := range candidates {
		expiresAt := candidate.ExpiresAt.UTC()
		if candidate.TaskID == 0 || candidate.UserID == 0 || candidate.StagedObjectID == 0 || !slices.Contains(statuses, candidate.Status) ||
			expiresAt.Year() < 1000 || expiresAt.Year() > 9999 || expiresAt.Nanosecond()%1000 != 0 ||
			expiresAt.Before(previousExpiry) || (expiresAt.Equal(previousExpiry) && candidate.TaskID <= previousTaskID) {
			return nil, domain.ErrInvalidUploadTaskData
		}
		candidates[index].ExpiresAt = expiresAt
		previousExpiry, previousTaskID = expiresAt, candidate.TaskID
	}
	return candidates, nil
}
