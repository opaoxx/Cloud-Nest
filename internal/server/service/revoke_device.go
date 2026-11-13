package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RevokeDeviceTransaction 列出指定设备撤销服务需要的事务内能力。
type RevokeDeviceTransaction interface {
	// RevokeUserDevice 按已绑定用户归属撤销目标，结果等待外层提交。
	RevokeUserDevice(ctx context.Context, targetDeviceID uint64) error
}

// RevokeDeviceService 组合已认证用户事务与指定设备撤销。
type RevokeDeviceService struct {
	// withTransaction 同步执行一次回调，传播错误，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, RevokeDeviceTransaction) error) error
}

// NewRevokeDeviceService 绑定已认证用户事务，缺少依赖属于装配错误。
func NewRevokeDeviceService(withTransaction func(context.Context, domain.Actor, func(context.Context, RevokeDeviceTransaction) error) error) *RevokeDeviceService {
	// 1. 检查并保存后续调用使用的事务方法。
	if withTransaction == nil {
		panic("指定设备撤销服务需要已认证用户事务方法")
	}
	return &RevokeDeviceService{withTransaction: withTransaction}
}

// Revoke 为已认证 Actor 撤销指定设备，事务失败返回空结果。
// 目标不存在在回调内转换，事务入口的调用者状态错误保留原有语义。
// 已确认提交后返回已提交结果，后续交付层继续处理取消。
func (revokeService *RevokeDeviceService) Revoke(ctx context.Context, actor domain.Actor, targetDeviceID uint64) (*domain.RevokeDeviceResult, error) {
	// 1. 检查认证身份、目标设备 ID 和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || targetDeviceID == 0 {
		return nil, fmt.Errorf("撤销指定设备需要上下文、完整认证身份和非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 在用户事务中撤销目标，区分目标错误并暂存结果。
	var prepared *domain.RevokeDeviceResult
	transactionError := revokeService.withTransaction(ctx, actor, func(txContext context.Context, txStore RevokeDeviceTransaction) error {
		// 1. 确认事务上下文与撤销能力完整且仍有效。
		if txContext == nil || txStore == nil {
			return fmt.Errorf("撤销指定设备回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		// 2. 使用同一事务撤销目标，先检查取消，再处理目标不存在。
		revokeError := txStore.RevokeUserDevice(txContext, targetDeviceID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if errors.Is(revokeError, domain.ErrDeviceNotFound) {
			return domain.ErrTargetDeviceNotFound
		}
		if revokeError != nil {
			return fmt.Errorf("撤销目标设备失败: %w", revokeError)
		}
		// 3. 暂存撤销结果，等待外层事务确认提交。
		prepared = &domain.RevokeDeviceResult{Revoked: true}
		return nil
	})

	// 3. 确认事务提交成功，返回撤销结果。
	if transactionError != nil {
		return nil, fmt.Errorf("撤销指定设备事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidDeviceData
	}
	return prepared, nil
}
