package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListExpiredFileVersionCandidates 只读返回保留期已到的历史引用，供后续所属账号事务复核。
func (store *Store) ListExpiredFileVersionCandidates(ctx context.Context, limit int) ([]domain.FileVersion, error) {
	// 1. 校验调用位置和批量大小。跨账号扫描使用事务外的仓储入口。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > domain.MaxExpiredFileVersionCandidates {
		return nil, fmt.Errorf("扫描到期历史版本需要数据入口、上下文和 1～%d 条的批量大小", domain.MaxExpiredFileVersionCandidates)
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨账号扫描到期历史版本须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 使用 MySQL 的 UTC 时钟和现有到期索引。历史期满独立于当前条目状态。
	candidates := make([]domain.FileVersion, 0, limit)
	queryError := store.database.WithContext(queryContext).Table("file_versions").
		Select("id", "user_id", "entry_id", "revision", "object_id", "file_size", "created_at", "retained_until").
		Where("retained_until <= UTC_TIMESTAMP(6)").Order("retained_until ASC, id ASC").Limit(limit).Find(&candidates).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描到期历史版本失败: %w", queryError)
	}

	// 3. 统一返回 UTC 时间，核对后续锁内复核所需的标识、时间和顺序。
	for index := range candidates {
		candidate := &candidates[index]
		candidate.CreatedAt = candidate.CreatedAt.UTC()
		candidate.RetainedUntil = candidate.RetainedUntil.UTC()
		if !candidate.IsValid() {
			return nil, domain.ErrInvalidFileVersionData
		}
		if index > 0 {
			previous := candidates[index-1]
			if candidate.RetainedUntil.Before(previous.RetainedUntil) ||
				(candidate.RetainedUntil.Equal(previous.RetainedUntil) && candidate.ID <= previous.ID) {
				return nil, domain.ErrInvalidFileVersionData
			}
		}
	}
	return candidates, nil
}
