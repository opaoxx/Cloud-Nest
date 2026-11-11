package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// AuthorizationService 组合令牌校验与数据库状态，判断普通请求能否继续执行。
type AuthorizationService struct {
	// verifyToken 提供第 034 步签名和声明校验能力。
	verifyToken func(string) (*domain.VerifiedAccessToken, error)
	// findSession 提供第 035 步数据库授权信息查询能力。
	findSession func(context.Context, uint64, uint64) (*domain.SessionAuthorization, error)
	// now 用于查询后的有效期复查，构造时绑定生产时钟。
	now func() time.Time
}

// NewAuthorizationService 保存授权所需能力，缺失依赖属于装配错误。
func NewAuthorizationService(
	verifyToken func(string) (*domain.VerifiedAccessToken, error),
	findSession func(context.Context, uint64, uint64) (*domain.SessionAuthorization, error),
) *AuthorizationService {
	// 1. 检查依赖并保存能力，绑定当前时间来源。
	if verifyToken == nil || findSession == nil {
		panic("请求授权服务需要令牌校验与会话查询方法")
	}
	return &AuthorizationService{verifyToken: verifyToken, findSession: findSession, now: time.Now}
}

// Authorize 校验访问令牌及数据库状态，成功后返回供请求执行使用的 Actor。
// 关键写入继续在用户事务内按当前状态复核。
func (authorizationService *AuthorizationService) Authorize(ctx context.Context, tokenText string) (*domain.Actor, error) {
	// 1. 检查上下文，校验访问令牌。
	if ctx == nil {
		return nil, fmt.Errorf("请求授权需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	identity, tokenError := authorizationService.verifyToken(tokenText)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tokenError != nil {
		return nil, tokenError
	}
	if identity == nil || identity.UserID == 0 ||
		identity.SessionID == 0 || identity.ExpiresAt.IsZero() {
		return nil, domain.ErrInvalidSessionData
	}

	// 2. 根据已验证标识查询数据库授权状态。
	authorization, queryError := authorizationService.findSession(ctx, identity.UserID, identity.SessionID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, domain.ErrSessionNotFound) {
		return nil, domain.ErrSessionNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取请求授权状态失败: %w", queryError)
	}

	// 3. 复查归属、账号状态、撤销状态与有效期。
	if authorization == nil ||
		authorization.UserID != identity.UserID || authorization.SessionID != identity.SessionID ||
		authorization.DeviceID == 0 || authorization.SessionExpiresAt.IsZero() {
		return nil, domain.ErrInvalidSessionData
	}
	switch authorization.UserStatus {
	case "ACTIVE":
	case "DISABLED":
		return nil, domain.ErrAccountDisabled
	default:
		return nil, domain.ErrInvalidAccountData
	}
	if authorization.SessionRevokedAt != nil {
		return nil, domain.ErrSessionRevoked
	}
	if authorization.DeviceRevokedAt != nil {
		return nil, domain.ErrDeviceRevoked
	}
	now := authorizationService.now().UTC()
	if !identity.ExpiresAt.After(now) {
		return nil, domain.ErrAccessTokenExpired
	}
	if !authorization.SessionExpiresAt.After(now) {
		return nil, domain.ErrSessionExpired
	}

	// 4. 确认请求仍有效，返回已授权身份。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &domain.Actor{
		UserID:    authorization.UserID,
		SessionID: authorization.SessionID,
		DeviceID:  authorization.DeviceID,
	}, nil
}
