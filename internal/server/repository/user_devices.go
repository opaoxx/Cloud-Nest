package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ListUserDevices 按已认证用户读取设备，beforeDeviceID 为 0 时读取首页。
// pageSize 范围为 1～100，后续页使用上一页最后一条设备 ID 作为上界。
func (store *Store) ListUserDevices(ctx context.Context, userID, beforeDeviceID uint64, pageSize int) (*domain.UserDevicePage, error) {
	// 1. 校验查询参数，设置查询期限。
	if ctx == nil || userID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("查询用户设备列表需要上下文、非零用户 ID 和 1～100 的页大小")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询用户设备列表失败: %w", err)
	}

	// 2. 按用户及设备 ID 上界读取一页和额外一条记录。
	query := store.database.WithContext(queryContext).
		Select("id", "user_id", "name", "platform", "created_at", "last_seen_at", "revoked_at").
		Where("user_id = ?", userID)
	if beforeDeviceID != 0 {
		query = query.Where("id < ?", beforeDeviceID)
	}
	var records []models.UserDevice
	queryError := query.Order("id DESC").Limit(pageSize + 1).Find(&records).Error

	// 3. 处理取消与数据库错误。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询用户设备列表已取消: %w", err)
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询用户设备列表失败: %w", queryError)
	}

	// 4. 核对归属，映射设备列表记录。
	devices := make([]domain.UserDevice, 0, len(records))
	for _, record := range records {
		if record.UserID != userID || record.ID == 0 {
			return nil, domain.ErrInvalidDeviceData
		}
		devices = append(devices, domain.UserDevice{
			ID:         record.ID,
			UserID:     record.UserID,
			Name:       record.Name,
			Platform:   record.Platform,
			CreatedAt:  record.CreatedAt,
			LastSeenAt: record.LastSeenAt,
			RevokedAt:  record.RevokedAt,
		})
	}

	// 5. 根据额外记录判断后续页，返回本页结果。
	hasMore := len(devices) > pageSize
	if hasMore {
		devices = devices[:pageSize]
	}
	return &domain.UserDevicePage{Devices: devices, HasMore: hasMore}, nil
}
