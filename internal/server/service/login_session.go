package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// LoginSessionTransaction 列出登录会话服务在同一用户事务内需要的 Repository 能力。
type LoginSessionTransaction interface {
	// EnsureLoginDevice 准备当前已锁定用户的设备，结果在事务提交后生效。
	EnsureLoginDevice(ctx context.Context, installationID, name, platform string) (*domain.LoginDevice, error)
	// CreateLoginSession 写入当前用户的会话，原文摘要与到期时间由服务提供。
	CreateLoginSession(ctx context.Context, deviceID uint64, tokenHash [sha256.Size]byte, expiresAt time.Time) (*domain.LoginSession, error)
}

// LoginSessionService 组合刷新凭证生成、设备准备与会话创建。
type LoginSessionService struct {
	// withTransaction 绑定用户事务入口，回调成功后提交，错误或异常时回滚。
	withTransaction func(context.Context, uint64, func(context.Context, LoginSessionTransaction) error) error
	// lifetime 保存构造时已经校验的刷新凭证与会话有效期。
	lifetime time.Duration
	// now 提供本次会话期限计算使用的时间。
	now func() time.Time
	// generateRefreshToken 绑定安全随机生成方法，原文用于提交后的交付。
	generateRefreshToken func() (string, [sha256.Size]byte)
}

// NewLoginSessionService 校验配置并绑定事务能力，调用时复用同一服务。
// withTransaction 须同步执行一次回调，传播回调错误，并仅在提交成功时返回 nil。
func NewLoginSessionService(
	jwtConfig config.JWTConfig,
	withTransaction func(context.Context, uint64, func(context.Context, LoginSessionTransaction) error) error,
) (*LoginSessionService, error) {
	// 1. 共用 JWT 配置校验，确认事务入口已经提供。
	if err := jwtConfig.Validate(); err != nil {
		return nil, err
	}
	if withTransaction == nil {
		return nil, fmt.Errorf("登录会话服务需要用户事务方法")
	}

	// 2. 保存会话有效期，绑定时钟及刷新凭证生成能力。
	return &LoginSessionService{
		withTransaction:      withTransaction,
		lifetime:             jwtConfig.RefreshTokenTTL,
		now:                  time.Now,
		generateRefreshToken: GenerateRefreshToken,
	}, nil
}

// Create 为已验证账号准备设备并创建会话，输入字段须已通过登录 DTO 校验。
// 事务返回错误时结果为 nil，提交失败不自动重试。
// 事务确认提交后返回已提交结果，请求层继续处理后续取消或响应交付。
func (sessionService *LoginSessionService) Create(
	ctx context.Context,
	account domain.VerifiedAccount,
	installationID, deviceName, platform string,
) (*domain.LoginSessionResult, error) {
	// 1. 检查已验证账号和请求上下文。
	if ctx == nil || account.ID == 0 {
		return nil, fmt.Errorf("创建登录会话需要上下文和已验证账号")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 生成刷新凭证及摘要，计算会话到期时间。
	rawToken, tokenHash := sessionService.generateRefreshToken()
	now := sessionService.now().UTC()
	expiresAt := now.Add(sessionService.lifetime).Truncate(time.Microsecond)
	if expiresAt.Year() < 1000 || expiresAt.Year() > 9999 || !expiresAt.After(now) {
		return nil, domain.ErrInvalidSessionData
	}

	// 3. 在同一用户事务中准备设备并创建会话。
	var prepared *domain.LoginSessionResult
	transactionError := sessionService.withTransaction(ctx, account.ID, func(txContext context.Context, txStore LoginSessionTransaction) error {
		// 1. 使用事务入口传入的上下文与 Store，准备本次登录设备。
		if txContext == nil || txStore == nil {
			return fmt.Errorf("登录会话回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		device, deviceError := txStore.EnsureLoginDevice(txContext, installationID, deviceName, platform)
		if err := txContext.Err(); err != nil {
			return err
		}
		if deviceError != nil {
			return fmt.Errorf("准备登录会话设备失败: %w", deviceError)
		}
		if device == nil || device.ID == 0 || device.UserID != account.ID ||
			device.InstallationID != installationID || device.RevokedAt != nil {
			return domain.ErrInvalidDeviceData
		}

		// 2. 将摘要与到期时间写入同一事务，检查返回会话的归属与期限。
		session, sessionError := txStore.CreateLoginSession(txContext, device.ID, tokenHash, expiresAt)
		if err := txContext.Err(); err != nil {
			return err
		}
		if sessionError != nil {
			return fmt.Errorf("写入登录会话失败: %w", sessionError)
		}
		if session == nil || session.ID == 0 || session.UserID != account.ID ||
			session.DeviceID != device.ID || !session.ExpiresAt.Equal(expiresAt) {
			return domain.ErrInvalidSessionData
		}

		// 3. 暂存结果副本，等待外层用户事务确认提交。
		prepared = &domain.LoginSessionResult{Device: *device, Session: *session, RefreshToken: rawToken}
		return nil
	})

	// 4. 确认事务提交成功，返回登录会话结果。
	if transactionError != nil {
		return nil, fmt.Errorf("创建登录会话事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidSessionData
	}
	return prepared, nil
}
