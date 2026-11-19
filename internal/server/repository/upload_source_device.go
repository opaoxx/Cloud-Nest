package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// FindUploadSourceDevice 为已复核上传任务读取同账号设备的发布快照。
// 使用用户事务的 Store 和上下文，已撤销设备也可作为历史来源返回。
func (store *Store) FindUploadSourceDevice(ctx context.Context, task *domain.UploadTask) (*domain.UserDevice, error) {
	// 1. 核对当前用户事务和任务的来源设备 ID。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil {
		return nil, fmt.Errorf("读取上传来源设备需要上下文和已锁定用户的事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("读取上传来源设备必须使用事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	userID := store.loginUserID
	if task == nil || task.ID == 0 || task.UserID != userID || task.DeviceID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	deviceID := task.DeviceID

	// 2. 按事务用户及设备 ID 读取设备记录。
	// 设备改名及撤销使用同一用户锁，发布事务内可稳定读取来源名称。
	var record models.UserDevice
	queryError := store.database.WithContext(ctx).
		Select("id", "user_id", "name", "platform", "created_at", "last_seen_at", "revoked_at").
		Where("user_id = ? AND id = ?", userID, deviceID).Take(&record).Error

	// 3. 区分缺失和查询故障，校验后返回设备快照。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDeviceNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取上传来源设备失败: %w", queryError)
	}
	if record.ID != deviceID || record.UserID != userID || !domain.ValidDeviceName(record.Name) || record.CreatedAt.IsZero() {
		return nil, domain.ErrInvalidDeviceData
	}
	return &domain.UserDevice{
		ID:         record.ID,
		UserID:     record.UserID,
		Name:       record.Name,
		Platform:   record.Platform,
		CreatedAt:  record.CreatedAt,
		LastSeenAt: record.LastSeenAt,
		RevokedAt:  record.RevokedAt,
	}, nil
}
