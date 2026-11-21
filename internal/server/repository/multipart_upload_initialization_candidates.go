package repository

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListMultipartUploadInitializationCandidates 在事务外读取可领取的分片初始化候选。
// 候选供内部 worker 使用，领取租约时仍须按所属用户重新核对。
func (store *Store) ListMultipartUploadInitializationCandidates(ctx context.Context, limit int) ([]domain.MultipartUploadInitializationCandidate, error) {
	// 1. 校验读取入口和批量大小，限制单次查询等待时间。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > maxUploadLeaseCandidates {
		return nil, fmt.Errorf("扫描分片初始化需要数据入口、上下文和 1～100 条的批量大小")
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描分片初始化须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 在限量前筛选尚未建立存储会话的分片任务，使用数据库时间。
	candidates := make([]domain.MultipartUploadInitializationCandidate, 0, limit)
	query := store.database.WithContext(queryContext).
		Where("status = 'CREATING' AND upload_mode = 'MULTIPART' AND storage_upload_id IS NULL")
	queryError := availableUploadLeaseCandidates(query, limit).Find(&candidates).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描分片初始化候选失败: %w", queryError)
	}

	// 3. 核对候选定位和顺序，交给后续领取事务复查。
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
