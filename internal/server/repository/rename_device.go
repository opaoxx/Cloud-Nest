package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RenameUserDevice 在已认证用户事务中修改自己的指定设备名称，保留名称原文。
// 目标缺失或属于其他账号时返回 ErrDeviceNotFound，已撤销目标也可改名并保留撤销状态。
// ctx 与 Store 来自 WithUserTransaction，返回值在外层事务提交成功后才能确认生效。
func (store *Store) RenameUserDevice(ctx context.Context, targetDeviceID uint64, name string) (*domain.UserDevice, error) {
	// 1. 检查已认证事务、目标 ID、名称和上下文。
	if ctx == nil || targetDeviceID == 0 || !domain.ValidDeviceName(name) || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("修改设备名称需要上下文、非零目标 ID、合法名称和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("修改设备名称必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	updateContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := updateContext.Err(); err != nil {
		return nil, err
	}

	// 2. 只读确认目标归属，避免等待其他账号的设备锁。
	var scope models.UserDevice
	queryError := store.database.WithContext(updateContext).
		Select("id", "user_id").Where("user_id = ? AND id = ?", actor.UserID, targetDeviceID).Take(&scope).Error
	if err := updateContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDeviceNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("定位待改名设备失败: %w", queryError)
	}
	if scope.ID != targetDeviceID || scope.UserID != actor.UserID {
		return nil, domain.ErrInvalidDeviceData
	}

	// 3. 锁定目标设备，复查归属并读取设备信息。
	// 外层持有用户锁，同账号认证写入按用户锁串行，此处读取目标的当前状态。
	var target models.UserDevice
	queryError = store.database.WithContext(updateContext).
		Select("id", "user_id", "name", "platform", "created_at", "last_seen_at", "revoked_at").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND id = ?", actor.UserID, targetDeviceID).Take(&target).Error
	if err := updateContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDeviceNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("锁定待改名设备失败: %w", queryError)
	}
	if target.ID != targetDeviceID || target.UserID != actor.UserID || target.CreatedAt.IsZero() {
		return nil, domain.ErrInvalidDeviceData
	}

	// 4. 名称变化时更新 name 和数据库 UTC 更新时间。
	if target.Name != name {
		result := store.database.WithContext(updateContext).Exec(
			"UPDATE user_devices SET name = ?, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ?",
			name, actor.UserID, targetDeviceID)
		if err := updateContext.Err(); err != nil {
			return nil, err
		}
		if result.Error != nil {
			return nil, fmt.Errorf("修改设备名称失败: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return nil, domain.ErrInvalidDeviceData
		}
	}

	// 5. 返回事务内结果，交由外层提交或回滚。
	if err := updateContext.Err(); err != nil {
		return nil, err
	}
	return &domain.UserDevice{
		ID:         target.ID,
		UserID:     target.UserID,
		Name:       name,
		Platform:   target.Platform,
		CreatedAt:  target.CreatedAt,
		LastSeenAt: target.LastSeenAt,
		RevokedAt:  target.RevokedAt,
	}, nil
}
