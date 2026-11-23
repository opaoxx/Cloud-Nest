package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// EnsureLoginDevice 在登录事务中取得可用于本次登录的设备，缺失时创建，撤销后重新登录时恢复。
// 入口须已验证账号密码，字段须已通过 DTO 校验，ctx 和 Store 来自 WithLoginUserTransaction。
// 结果在外层事务提交成功后才生效。
func (store *Store) EnsureLoginDevice(ctx context.Context, installationID, name, platform string) (*domain.LoginDevice, error) {
	// 1. 确认登录用户事务和上下文有效。
	if ctx == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("准备登录设备需要上下文和已锁定用户的登录事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("准备登录设备必须使用登录事务 Store")
	}
	deviceContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := deviceContext.Err(); err != nil {
		return nil, err
	}

	// 2. 在当前用户下查询安装设备，不存在时创建。
	device, queryError := store.FindLoginDeviceByInstallation(deviceContext, store.loginUserID, installationID)
	if err := deviceContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, domain.ErrDeviceNotFound) {
		return store.CreateLoginDevice(deviceContext, installationID, name, platform)
	}
	if queryError != nil {
		return nil, fmt.Errorf("准备登录设备查询失败: %w", queryError)
	}

	// 3. 检查已有设备的归属，普通设备直接复用。
	if device == nil || device.ID == 0 || device.UserID != store.loginUserID || device.InstallationID != installationID {
		return nil, domain.ErrInvalidDeviceData
	}
	if device.RevokedAt == nil {
		return device, nil
	}

	// 4. 恢复已撤销设备，条件更新同时锁住该设备行。
	result := store.database.WithContext(deviceContext).Exec(
		"UPDATE user_devices SET revoked_at = NULL, updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND id = ? AND revoked_at IS NOT NULL",
		store.loginUserID, device.ID)
	if result.Error != nil {
		return nil, fmt.Errorf("恢复登录设备失败: %w", result.Error)
	}
	if err := deviceContext.Err(); err != nil {
		return nil, err
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidDeviceData
	}

	// 5. 在同一事务使旧同步代次失效，错误交给外层回滚。
	if err := store.invalidateRevokedDeviceSyncState(deviceContext, device.UserID, device.ID); err != nil {
		return nil, err
	}
	restoredDevice := *device
	restoredDevice.RevokedAt = nil
	return &restoredDevice, nil
}
