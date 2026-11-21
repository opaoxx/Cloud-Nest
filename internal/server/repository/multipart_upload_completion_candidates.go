package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListMultipartUploadCompletionCandidates 在事务外读取可领取后台合并租约的候选。
// 后续领取租约时仍须在所属用户事务中重新核对当前任务。
func (store *Store) ListMultipartUploadCompletionCandidates(ctx context.Context, limit int) ([]domain.MultipartUploadCompletionCandidate, error) {
	// 1. 校验扫描入口、上下文和批量上限。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > domain.MaxMultipartUploadCompletionCandidates {
		return nil, fmt.Errorf("扫描分片合并需要数据入口、上下文和 1～%d 条的批量大小", domain.MaxMultipartUploadCompletionCandidates)
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描分片合并须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 按数据库时间筛选已绑定会话的 COMPLETING 任务。
	candidates := make([]domain.MultipartUploadCompletionCandidate, 0, limit)
	query := store.database.WithContext(queryContext).
		Where("status = 'COMPLETING' AND upload_mode = 'MULTIPART' AND TRIM(storage_upload_id) <> ''")
	queryError := availableUploadLeaseCandidates(query, limit).Find(&candidates).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描分片合并候选失败: %w", queryError)
	}

	// 3. 核对返回的定位与顺序，交给后续领取事务复查。
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
