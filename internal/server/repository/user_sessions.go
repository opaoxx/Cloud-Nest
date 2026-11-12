package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ListUserSessions 按已认证用户读取会话，beforeSessionID 为 0 时读取首页。
// pageSize 范围为 1～100，后续页使用上一页最后一条会话 ID 作为上界。
func (store *Store) ListUserSessions(ctx context.Context, userID, beforeSessionID uint64, pageSize int) (*domain.UserSessionPage, error) {
	// 1. 校验查询参数，设置查询期限。
	if ctx == nil || userID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("查询用户会话列表需要上下文、非零用户 ID 和 1～100 的页大小")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询用户会话列表失败: %w", err)
	}

	// 2. 按用户关联设备，倒序读取一页及额外一条记录。
	query := store.database.WithContext(queryContext).
		Table("auth_sessions AS sessions").
		Select([]string{
			"sessions.id AS id", "sessions.user_id AS user_id", "sessions.device_id AS device_id",
			"devices.name AS device_name", "devices.platform AS device_platform",
			"sessions.created_at AS created_at", "sessions.expires_at AS expires_at",
			"sessions.last_seen_at AS last_seen_at", "sessions.revoked_at AS revoked_at",
			"devices.revoked_at AS device_revoked_at",
		}).
		Joins("JOIN user_devices AS devices ON devices.user_id = sessions.user_id AND devices.id = sessions.device_id").
		Where("sessions.user_id = ?", userID)
	if beforeSessionID != 0 {
		query = query.Where("sessions.id < ?", beforeSessionID)
	}
	var records []models.UserSession
	queryError := query.Order("sessions.id DESC").Limit(pageSize + 1).Find(&records).Error

	// 3. 处理取消与数据库错误。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询用户会话列表已取消: %w", err)
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询用户会话列表失败: %w", queryError)
	}

	// 4. 核对归属，映射会话列表记录。
	sessions := make([]domain.UserSession, 0, len(records))
	for _, record := range records {
		if record.UserID != userID || record.ID == 0 || record.DeviceID == 0 {
			return nil, domain.ErrInvalidSessionData
		}
		sessions = append(sessions, domain.UserSession{
			ID:              record.ID,
			UserID:          record.UserID,
			DeviceID:        record.DeviceID,
			DeviceName:      record.DeviceName,
			DevicePlatform:  record.DevicePlatform,
			CreatedAt:       record.CreatedAt,
			ExpiresAt:       record.ExpiresAt,
			LastSeenAt:      record.LastSeenAt,
			RevokedAt:       record.RevokedAt,
			DeviceRevokedAt: record.DeviceRevokedAt,
		})
	}

	// 5. 根据额外记录判断后续页，返回本页结果。
	hasMore := len(sessions) > pageSize
	if hasMore {
		sessions = sessions[:pageSize]
	}
	return &domain.UserSessionPage{Sessions: sessions, HasMore: hasMore}, nil
}
