package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// FindSyncDeviceGeneration 按可信账号和设备读取当前已提交的同步代次。
// 调用方须先检查认证会话。使用事务外入口，避免读到旧快照或本事务尚未提交的代次。
func (store *Store) FindSyncDeviceGeneration(ctx context.Context, userID, deviceID uint64) (domain.SyncCursorGeneration, error) {
	// 1. 核对身份参数和读取入口，设置最多五秒的查询期限。
	var empty domain.SyncCursorGeneration
	if ctx == nil || userID == 0 || deviceID == 0 {
		return empty, fmt.Errorf("查询同步代次需要上下文及非零账号和设备 ID")
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return empty, fmt.Errorf("读取当前同步代次需要事务外的查询入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return empty, err
	}

	// 2. 按共同归属关联设备与同步记录，保留尚未建立同步记录的设备。
	var record struct {
		UserID           uint64
		DeviceID         uint64
		RevokedAt        *time.Time
		SyncDeviceID     *uint64
		CursorGeneration []byte
	}
	queryError := store.database.WithContext(queryContext).
		Table("user_devices AS devices").
		Select("devices.user_id, devices.id AS device_id, devices.revoked_at, states.device_id AS sync_device_id, states.cursor_generation").
		Joins("LEFT JOIN sync_devices AS states ON states.user_id = devices.user_id AND states.device_id = devices.id").
		Where("devices.user_id = ? AND devices.id = ?", userID, deviceID).
		Take(&record).Error
	if err := queryContext.Err(); err != nil {
		return empty, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return empty, domain.ErrDeviceNotFound
	}
	if queryError != nil {
		return empty, fmt.Errorf("查询同步代次失败: %w", queryError)
	}

	// 3. 区分撤销、无同步记录及异常代次，按值返回完整 16 字节。
	if record.UserID != userID || record.DeviceID != deviceID {
		return empty, domain.ErrInvalidDeviceData
	}
	if record.RevokedAt != nil {
		return empty, domain.ErrDeviceRevoked
	}
	if record.SyncDeviceID == nil {
		return empty, domain.ErrSyncDeviceNotFound
	}
	if *record.SyncDeviceID != deviceID || len(record.CursorGeneration) != len(empty) {
		return empty, domain.ErrInvalidSyncDeviceData
	}
	generation := domain.SyncCursorGeneration(record.CursorGeneration)
	if generation == empty {
		return empty, domain.ErrInvalidSyncDeviceData
	}
	return generation, nil
}
