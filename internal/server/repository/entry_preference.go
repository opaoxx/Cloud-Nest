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

// FindUserEntryPreference 按已验证用户归属读取指定目录项的偏好事实。
// 已有目录项缺少偏好记录时，两项时间为 nil。缺失或跨账号目标返回 ErrFileEntryNotFound。
// 后续详情服务在同一快照中组合账号状态和目录项祖先可见性检查。
func (store *Store) FindUserEntryPreference(ctx context.Context, userID, entryID uint64) (*domain.EntryPreference, error) {
	// 1. 检查用户、目录项 ID 和上下文，设置查询期限。
	if ctx == nil || userID == 0 || entryID == 0 {
		return nil, fmt.Errorf("查询目录项偏好需要上下文、非零用户 ID 和目录项 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询目录项偏好失败: %w", err)
	}

	// 2. 按用户归属关联读取目录项及可空偏好。
	var record models.EntryPreference
	queryError := store.database.WithContext(queryContext).
		Table("file_entries AS entries").
		Select([]string{
			"entries.user_id AS user_id", "entries.id AS entry_id",
			"preferences.starred_at AS starred_at", "preferences.last_opened_at AS last_opened_at",
		}).
		Joins("LEFT JOIN user_entry_preferences AS preferences ON preferences.user_id = entries.user_id AND preferences.entry_id = entries.id").
		Where("entries.user_id = ? AND entries.id = ?", userID, entryID).
		Take(&record).Error

	// 3. 处理取消、目标不存在及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询目录项偏好已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询目录项偏好失败: %w", queryError)
	}

	// 4. 核对归属，返回收藏与最近访问时间。
	if record.UserID != userID || record.EntryID != entryID {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &domain.EntryPreference{
		UserID:       record.UserID,
		EntryID:      record.EntryID,
		StarredAt:    record.StarredAt,
		LastOpenedAt: record.LastOpenedAt,
	}, nil
}
