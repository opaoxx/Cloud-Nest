package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// LoginTokenService 在会话提交成功后签发访问令牌，组合两类凭证结果。
type LoginTokenService struct {
	// createSession 绑定 LoginSessionService.Create，成功结果已经完成事务提交。
	createSession func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginSessionResult, error)
	// issueAccessToken 绑定 AccessTokenSigner.Issue，接收已提交会话。
	issueAccessToken func(domain.LoginSession) (*domain.AccessToken, error)
}

// NewLoginTokenService 保存会话创建与令牌签发能力，缺少依赖属于装配错误。
func NewLoginTokenService(
	createSession func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginSessionResult, error),
	issueAccessToken func(domain.LoginSession) (*domain.AccessToken, error),
) *LoginTokenService {
	// 1. 确认依赖完整，保存已有服务的调用方法。
	if createSession == nil || issueAccessToken == nil {
		panic("登录凭证服务需要会话创建与访问令牌签发方法")
	}
	return &LoginTokenService{createSession: createSession, issueAccessToken: issueAccessToken}
}

// Create 为已验证账号创建会话并签发访问令牌，设备字段须已通过 DTO 校验。
// 失败返回 nil，已提交会话保留其提交状态，错误原因向上传递。
func (tokenService *LoginTokenService) Create(
	ctx context.Context,
	account domain.VerifiedAccount,
	installationID, deviceName, platform string,
) (*domain.LoginTokenResult, error) {
	// 1. 检查账号与请求上下文。
	if ctx == nil || account.ID == 0 {
		return nil, fmt.Errorf("创建登录凭证需要上下文和已验证账号")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 创建并取得已提交的登录会话。
	sessionResult, sessionError := tokenService.createSession(ctx, account, installationID, deviceName, platform)
	if sessionError != nil {
		return nil, fmt.Errorf("创建登录凭证会话失败: %w", sessionError)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sessionResult == nil {
		return nil, domain.ErrInvalidSessionData
	}
	created := *sessionResult
	if created.Device.ID == 0 || created.Device.UserID != account.ID ||
		created.Device.InstallationID != installationID || created.Device.RevokedAt != nil ||
		created.Session.ID == 0 || created.Session.UserID != account.ID ||
		created.Session.DeviceID != created.Device.ID || created.Session.ExpiresAt.IsZero() ||
		created.RefreshToken == "" {
		return nil, domain.ErrInvalidSessionData
	}

	// 3. 使用已提交会话签发 Access Token。
	accessToken, tokenError := tokenService.issueAccessToken(created.Session)
	if tokenError != nil {
		return nil, fmt.Errorf("签发登录访问令牌失败: %w", tokenError)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accessToken == nil || accessToken.Token == "" || accessToken.ExpiresAt.IsZero() ||
		accessToken.ExpiresAt.After(created.Session.ExpiresAt) {
		return nil, fmt.Errorf("登录访问令牌签发结果异常")
	}

	// 4. 返回包含两类凭证的登录结果。
	return &domain.LoginTokenResult{
		Device:       created.Device,
		Session:      created.Session,
		AccessToken:  *accessToken,
		RefreshToken: created.RefreshToken,
	}, nil
}
