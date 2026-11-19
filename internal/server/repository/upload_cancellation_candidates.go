package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

const maxUploadCancellationCandidates = 100

// ListSingleUploadCancellationCandidates 读取可接续收尾的单文件取消候选。
// 扫描在事务外执行，后续收尾必须重新核对任务与对象。
func (store *Store) ListSingleUploadCancellationCandidates(ctx context.Context, limit int) ([]domain.UploadCancellationCandidate, error) {
	// 1. 校验事务外入口、上下文和批量上限。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > maxUploadCancellationCandidates {
		return nil, fmt.Errorf("扫描待取消收尾上传需要数据入口、上下文和 1～100 条的批量大小")
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描待取消收尾上传须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 用 MySQL 时间筛出可收尾的单文件取消任务，再按 ID 限量。
	// 取消已经受理，只有清理租约和重试时间影响何时可以收尾。
	candidates := make([]domain.UploadCancellationCandidate, 0, limit)
	queryError := store.database.WithContext(queryContext).Table("upload_tasks").
		Select("id AS task_id", "user_id", "staged_object_id", "lease_version").
		Where("status = 'CANCELLING' AND upload_mode = 'SINGLE'").
		Where("(lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6))").
		Where("(next_attempt_at IS NULL OR next_attempt_at <= UTC_TIMESTAMP(6))").
		Order("id ASC").Limit(limit).Find(&candidates).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描待取消收尾上传失败: %w", queryError)
	}

	// 3. 核对定位字段和顺序，交给后续收尾事务复核。
	if len(candidates) > limit {
		return nil, domain.ErrInvalidUploadTaskData
	}
	var previousTaskID uint64
	for _, candidate := range candidates {
		if candidate.TaskID <= previousTaskID || candidate.UserID == 0 || candidate.StagedObjectID == 0 {
			return nil, domain.ErrInvalidUploadTaskData
		}
		previousTaskID = candidate.TaskID
	}
	return candidates, nil
}
