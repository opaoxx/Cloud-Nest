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

// FindUserFileEntryDisplay 按已验证用户归属读取指定目录项的扩展展示事实。
// userID 取自已认证调用方，后续详情服务在同一快照中组合路径可见性检查。
// 缺失或跨账号目标返回 ErrFileEntryNotFound，保留字段原文与可空语义。
func (store *Store) FindUserFileEntryDisplay(ctx context.Context, userID, entryID uint64) (*domain.FileEntryDisplay, error) {
	// 1. 检查用户、目录项 ID 和上下文，设置查询期限。
	if ctx == nil || userID == 0 || entryID == 0 {
		return nil, fmt.Errorf("查询目录项展示信息需要上下文、非零用户 ID 和目录项 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询目录项展示信息失败: %w", err)
	}

	// 2. 按用户归属读取目录项扩展展示字段。
	var record models.FileEntryDisplay
	queryError := store.database.WithContext(queryContext).
		Select("id", "user_id", "category", "extension", "mime_type", "source_device_id", "source_device_name", "color_key", "content_modified_at").
		Where("user_id = ? AND id = ?", userID, entryID).
		Take(&record).Error

	// 3. 处理取消、未找到及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询目录项展示信息已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询目录项展示信息失败: %w", queryError)
	}

	// 4. 核对归属和来源设备标识，映射可空展示字段。
	if record.ID != entryID || record.UserID != userID || (record.SourceDeviceID != nil && *record.SourceDeviceID == 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &domain.FileEntryDisplay{
		ID:                record.ID,
		UserID:            record.UserID,
		Category:          record.Category,
		Extension:         record.Extension,
		MIMEType:          record.MIMEType,
		SourceDeviceID:    record.SourceDeviceID,
		SourceDeviceName:  record.SourceDeviceName,
		ColorKey:          record.ColorKey,
		ContentModifiedAt: record.ContentModifiedAt,
	}, nil
}
