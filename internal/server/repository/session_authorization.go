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

// FindSessionAuthorization 只读查询指定用户与会话的状态，供请求授权及目标归属定位使用。
// userID 来自已验证凭证或事务身份，sessionID 为对应会话或用户范围内的目标。
// 关键写入仍在用户事务中锁定并复核当前状态。
func (store *Store) FindSessionAuthorization(ctx context.Context, userID, sessionID uint64) (*domain.SessionAuthorization, error) {
	// 1. 检查查询参数，设置有期限的查询上下文。
	if ctx == nil || userID == 0 || sessionID == 0 {
		return nil, fmt.Errorf("查询会话授权信息需要上下文及非零用户、会话 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询会话授权信息失败: %w", err)
	}

	// 2. 按用户和会话 ID 关联查询所需状态。
	var record models.SessionAuthorization
	queryError := store.database.WithContext(queryContext).
		Table("auth_sessions AS sessions").
		Select([]string{
			"sessions.user_id AS user_id", "sessions.id AS session_id", "sessions.device_id AS device_id",
			"accounts.status AS user_status", "sessions.expires_at AS session_expires_at",
			"sessions.revoked_at AS session_revoked_at", "devices.revoked_at AS device_revoked_at",
		}).
		Joins("JOIN users AS accounts ON accounts.id = sessions.user_id").
		Joins("JOIN user_devices AS devices ON devices.user_id = sessions.user_id AND devices.id = sessions.device_id").
		Where("sessions.user_id = ? AND sessions.id = ?", userID, sessionID).
		Take(&record).Error

	// 3. 区分未找到、取消和数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询会话授权信息已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrSessionNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询会话授权信息失败: %w", queryError)
	}

	// 4. 检查结果归属，转换为内部授权信息。
	if record.UserID != userID || record.SessionID != sessionID || record.DeviceID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	return &domain.SessionAuthorization{
		UserID:           record.UserID,
		SessionID:        record.SessionID,
		DeviceID:         record.DeviceID,
		UserStatus:       record.UserStatus,
		SessionExpiresAt: record.SessionExpiresAt,
		SessionRevokedAt: record.SessionRevokedAt,
		DeviceRevokedAt:  record.DeviceRevokedAt,
	}, nil
}
