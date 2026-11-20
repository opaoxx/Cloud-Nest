package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListPurgeRootCandidates 只读扫描供后台处理的永久清理根项。
// 扫描跨用户进行，推进前应在所属用户事务内重新读取目标。
func (store *Store) ListPurgeRootCandidates(ctx context.Context, limit int) ([]domain.PurgeRootCandidate, error) {
	// 1. 校验事务外入口、批量大小和查询期限。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > domain.MaxPurgeRootCandidates {
		return nil, fmt.Errorf("扫描永久清理根项需要数据入口、上下文和 1～%d 条的批量大小", domain.MaxPurgeRootCandidates)
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描永久清理根项须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 按开始时间和 ID 限量读取 PURGING 根项。
	candidates := make([]domain.PurgeRootCandidate, 0, limit)
	queryError := store.database.WithContext(queryContext).Table("file_entries").
		Select("id AS entry_id", "user_id", "parent_id", "revision", "purge_started_at").
		Where("status = 'PURGING' AND purge_started_at IS NOT NULL AND parent_id IS NOT NULL").
		Order("purge_started_at ASC, id ASC").Limit(limit).Find(&candidates).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描永久清理根项失败: %w", queryError)
	}

	// 3. 核对候选字段与顺序，交给后续事务复核。
	if len(candidates) > limit {
		return nil, domain.ErrInvalidFileEntryData
	}
	for index := range candidates {
		candidate := &candidates[index]
		candidate.PurgeStartedAt = candidate.PurgeStartedAt.UTC()
		startedAt := candidate.PurgeStartedAt
		if !candidate.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		if index > 0 {
			previous := candidates[index-1]
			if startedAt.Before(previous.PurgeStartedAt) ||
				(startedAt.Equal(previous.PurgeStartedAt) && candidate.EntryID <= previous.EntryID) {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
	}
	return candidates, nil
}
