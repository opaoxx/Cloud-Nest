package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// FindUserPurgeProgressTarget 读取指定用户的目录项清理事实，供进度服务检查。
// userID 来自认证身份；需继续统计时，调用方使用同一只读快照。
func (store *Store) FindUserPurgeProgressTarget(ctx context.Context, userID, entryID uint64) (*domain.PurgeProgressTarget, error) {
	// 1. 检查输入和查询期限。
	if ctx == nil || userID == 0 || entryID == 0 {
		return nil, fmt.Errorf("查询清理进度目标需要上下文、非零用户 ID 和目录项 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询清理进度目标失败: %w", err)
	}

	// 2. 按用户与目标 ID 只读取进度所需列。
	var record models.PurgeProgressTarget
	queryError := store.database.WithContext(queryContext).
		Select("id", "user_id", "parent_id", "status", "purge_started_at", "purged_at").
		Where("user_id = ? AND id = ?", userID, entryID).Take(&record).Error

	// 3. 区分未找到、取消和数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询清理进度目标已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询清理进度目标失败: %w", queryError)
	}

	// 4. 核对标识并返回状态与可空时间。
	if record.ID != entryID || record.UserID != userID ||
		(record.ParentID != nil && (*record.ParentID == 0 || *record.ParentID == entryID)) {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &domain.PurgeProgressTarget{
		ID: record.ID, UserID: record.UserID, ParentID: record.ParentID, Status: record.Status,
		PurgeStartedAt: record.PurgeStartedAt, PurgedAt: record.PurgedAt,
	}, nil
}
