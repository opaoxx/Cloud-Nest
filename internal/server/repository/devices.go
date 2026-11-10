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

// FindLoginDeviceByInstallation 在指定用户范围内按安装标识只读查询设备。
// userID 应来自已验证身份，installationID 应先经 DTO 校验；本方法不创建或恢复设备。
// 未命中返回 nil/ErrDeviceNotFound，其他失败返回 nil/error；已撤销的记录仍正常返回。
func (store *Store) FindLoginDeviceByInstallation(ctx context.Context, userID uint64, installationID string) (*domain.LoginDevice, error) {
	// 1. 检查用户 ID 和查询上下文，设置最多 5 秒期限。
	if ctx == nil || userID == 0 {
		return nil, fmt.Errorf("查询登录设备需要上下文和非零用户 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询登录设备失败: %w", err)
	}

	// 2. 参数绑定用户 ID 和安装标识，只读取设备所需六列。
	var record models.LoginDevice
	err := store.database.WithContext(queryContext).
		Select("id", "user_id", "installation_id", "name", "platform", "revoked_at").
		Where("user_id = ? AND installation_id = ?", userID, installationID).
		Take(&record).Error

	// 3. 区分设备不存在与真正的查询故障，失败不返回部分设备。
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrDeviceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询登录设备失败: %w", err)
	}

	// 4. 转换为内部设备对象，保留撤销时间的 nil 与实际值。
	return &domain.LoginDevice{
		ID:             record.ID,
		UserID:         record.UserID,
		InstallationID: record.InstallationID,
		Name:           record.Name,
		Platform:       record.Platform,
		RevokedAt:      record.RevokedAt,
	}, nil
}
