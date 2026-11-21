package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

const maxUploadLeaseCandidates = 100

// ListUploadVerificationCandidates 按数据库时间读取首期单文件校验候选。
// 结果供内部 worker 使用。领取时在用户事务中重新核对任务和对象。
func (store *Store) ListUploadVerificationCandidates(ctx context.Context, limit int) ([]domain.UploadVerificationCandidate, error) {
	// 1. 校验查询入口、批量上限和上下文。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > maxUploadLeaseCandidates {
		return nil, fmt.Errorf("扫描待校验上传需要数据入口、上下文和 1～100 条的批量大小")
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描待校验上传须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 用数据库当前时间筛选可领取的任务。
	candidates := make([]domain.UploadVerificationCandidate, 0, limit)
	query := store.database.WithContext(queryContext).Where("status = 'VERIFYING' AND upload_mode = 'SINGLE'")
	queryError := availableUploadLeaseCandidates(query, limit).Find(&candidates).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描待校验上传失败: %w", queryError)
	}

	// 3. 核对标识与顺序，返回供领取使用的候选。
	if len(candidates) > limit {
		return nil, domain.ErrInvalidUploadTaskData
	}
	var previousTaskID uint64
	for _, candidate := range candidates {
		if candidate.TaskID <= previousTaskID || candidate.UserID == 0 || candidate.StagedObjectID == 0 ||
			candidate.LeaseVersion == math.MaxUint64 {
			return nil, domain.ErrInvalidUploadTaskData
		}
		previousTaskID = candidate.TaskID
	}
	return candidates, nil
}

func availableUploadLeaseCandidates(query *gorm.DB, limit int) *gorm.DB {
	// 1. 用同一 SQL 语句的当前时间核对任务、重试和租约期限。
	return query.Table("upload_tasks").
		Select("id AS task_id", "user_id", "staged_object_id", "lease_version").
		Where("expires_at > UTC_TIMESTAMP(6)").
		Where("(next_attempt_at IS NULL OR next_attempt_at <= UTC_TIMESTAMP(6))").
		Where("(lease_until IS NULL OR lease_until <= UTC_TIMESTAMP(6)) AND lease_version < ?", uint64(math.MaxUint64)).
		Order("id ASC").Limit(limit)
}
