package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// LogoutTransaction 列出退出服务在已认证事务中需要的会话撤销能力。
type LogoutTransaction interface {
	// RevokeCurrentSession 撤销当前绑定会话，结果等待外层提交。
	RevokeCurrentSession(ctx context.Context) error
}

// LogoutService 组合已认证用户事务与当前会话撤销。
type LogoutService struct {
	// withTransaction 同步执行一次回调，传播错误，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, LogoutTransaction) error) error
}

// NewLogoutService 绑定已认证用户事务，缺少依赖属于装配错误。
func NewLogoutService(withTransaction func(context.Context, domain.Actor, func(context.Context, LogoutTransaction) error) error) *LogoutService {
	// 1. 检查并保存后续调用使用的事务方法。
	if withTransaction == nil {
		panic("退出登录服务需要已认证用户事务方法")
	}
	return &LogoutService{withTransaction: withTransaction}
}

// Logout 为已认证 Actor 撤销当前会话，事务失败返回空结果并保留原因。
// 已确认提交后返回已提交结果，后续交付层继续处理取消。
func (logoutService *LogoutService) Logout(ctx context.Context, actor domain.Actor) (*domain.LogoutResult, error) {
	// 1. 检查已认证身份与请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, fmt.Errorf("退出登录需要上下文和完整已认证身份")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入用户事务，撤销当前绑定会话。
	var prepared *domain.LogoutResult
	transactionError := logoutService.withTransaction(ctx, actor, func(txContext context.Context, txStore LogoutTransaction) error {
		// 1. 确认事务上下文与撤销能力完整且仍有效。
		if txContext == nil || txStore == nil {
			return fmt.Errorf("退出登录回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		// 2. 使用同一事务撤销会话，处理取消与写入错误。
		revokeError := txStore.RevokeCurrentSession(txContext)
		if err := txContext.Err(); err != nil {
			return err
		}
		if revokeError != nil {
			return fmt.Errorf("退出登录撤销会话失败: %w", revokeError)
		}
		// 3. 暂存撤销结果，等待外层事务确认提交。
		prepared = &domain.LogoutResult{Revoked: true}
		return nil
	})

	// 3. 确认事务提交成功，返回撤销结果。
	if transactionError != nil {
		return nil, fmt.Errorf("退出登录事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidSessionData
	}
	return prepared, nil
}
