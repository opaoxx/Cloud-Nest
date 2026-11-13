package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RenameDeviceTransaction 列出设备改名服务需要的事务内能力。
type RenameDeviceTransaction interface {
	// RenameUserDevice 更新当前用户的目标设备名称，结果等待外层提交。
	RenameUserDevice(ctx context.Context, targetDeviceID uint64, name string) (*domain.UserDevice, error)
}

// RenameDeviceService 组合已认证用户事务与指定设备名称更新。
type RenameDeviceService struct {
	// withTransaction 同步执行一次回调，传播错误，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, RenameDeviceTransaction) error) error
}

// NewRenameDeviceService 绑定已认证用户事务，缺少依赖属于装配错误。
func NewRenameDeviceService(withTransaction func(context.Context, domain.Actor, func(context.Context, RenameDeviceTransaction) error) error) *RenameDeviceService {
	// 1. 检查并保存后续调用使用的事务方法。
	if withTransaction == nil {
		panic("设备改名服务需要已认证用户事务方法")
	}
	return &RenameDeviceService{withTransaction: withTransaction}
}

// Rename 为已认证 Actor 修改指定设备名称，事务失败返回空结果。
// 目标错误在回调内转换，事务入口的调用者状态错误保留原有语义。
// 已确认提交后返回独立设备结果，后续交付层继续处理取消。
func (renameService *RenameDeviceService) Rename(ctx context.Context, actor domain.Actor, targetDeviceID uint64, name string) (*domain.UserDevice, error) {
	// 1. 检查认证身份、目标设备、名称和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || targetDeviceID == 0 || !domain.ValidDeviceName(name) {
		return nil, fmt.Errorf("修改设备名称需要上下文、完整认证身份、非零目标 ID 和合法名称")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入用户事务，执行改名并区分目标错误。
	var prepared *domain.UserDevice
	transactionError := renameService.withTransaction(ctx, actor, func(txContext context.Context, txStore RenameDeviceTransaction) error {
		// 1. 确认事务上下文与改名能力完整且仍有效。
		if txContext == nil || txStore == nil {
			return fmt.Errorf("设备改名回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		// 2. 执行改名，先检查取消，再区分目标错误。
		device, renameError := txStore.RenameUserDevice(txContext, targetDeviceID, name)
		if err := txContext.Err(); err != nil {
			return err
		}
		if errors.Is(renameError, domain.ErrDeviceNotFound) {
			return domain.ErrTargetDeviceNotFound
		}
		if renameError != nil {
			return fmt.Errorf("修改目标设备名称失败: %w", renameError)
		}

		// 3. 核对并复制设备结果，等待事务提交。
		if device == nil || device.ID != targetDeviceID || device.UserID != actor.UserID || device.Name != name || device.CreatedAt.IsZero() {
			return domain.ErrInvalidDeviceData
		}
		deviceCopy := *device
		if device.LastSeenAt != nil {
			lastSeenAt := *device.LastSeenAt
			deviceCopy.LastSeenAt = &lastSeenAt
		}
		if device.RevokedAt != nil {
			revokedAt := *device.RevokedAt
			deviceCopy.RevokedAt = &revokedAt
		}
		prepared = &deviceCopy
		return nil
	})

	// 4. 确认提交成功，返回设备更新结果。
	if transactionError != nil {
		return nil, fmt.Errorf("设备改名事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidDeviceData
	}
	return prepared, nil
}
