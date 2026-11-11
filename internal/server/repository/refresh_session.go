package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// FindRefreshSession 按刷新原文的 SHA256 摘要读取会话定位与授权状态。
// 轮换写入须在用户事务中复查当前摘要、代次与授权状态。
func (store *Store) FindRefreshSession(ctx context.Context, tokenHash [sha256.Size]byte) (*domain.RefreshSession, error) {
	// 1. 检查上下文，设置查询期限。
	if ctx == nil {
		return nil, fmt.Errorf("查询刷新会话需要上下文")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询刷新会话失败: %w", err)
	}

	// 2. 按摘要关联查询会话、用户和设备状态。
	var record models.RefreshSession
	queryError := store.database.WithContext(queryContext).
		Table("auth_sessions AS sessions").
		Select([]string{
			"sessions.user_id AS user_id", "sessions.id AS session_id", "sessions.device_id AS device_id",
			"accounts.status AS user_status", "sessions.expires_at AS session_expires_at",
			"sessions.revoked_at AS session_revoked_at", "devices.revoked_at AS device_revoked_at",
			"sessions.token_generation AS token_generation",
		}).
		Joins("JOIN users AS accounts ON accounts.id = sessions.user_id").
		Joins("JOIN user_devices AS devices ON devices.user_id = sessions.user_id AND devices.id = sessions.device_id").
		Where("sessions.refresh_token_hash = ?", tokenHash[:]).
		Take(&record).Error

	// 3. 处理取消、未找到及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询刷新会话已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrSessionNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询刷新会话失败: %w", queryError)
	}

	// 4. 检查标识与代次，返回刷新会话快照。
	if record.UserID == 0 || record.SessionID == 0 || record.DeviceID == 0 || record.TokenGeneration == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	return &domain.RefreshSession{
		SessionAuthorization: domain.SessionAuthorization{
			UserID:           record.UserID,
			SessionID:        record.SessionID,
			DeviceID:         record.DeviceID,
			UserStatus:       record.UserStatus,
			SessionExpiresAt: record.SessionExpiresAt,
			SessionRevokedAt: record.SessionRevokedAt,
			DeviceRevokedAt:  record.DeviceRevokedAt,
		},
		TokenGeneration: record.TokenGeneration,
	}, nil
}
