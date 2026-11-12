package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RefreshTokenService 在刷新事务提交后签发访问令牌，组合两类新凭证。
type RefreshTokenService struct {
	// refreshSession 绑定 RefreshSessionService.Refresh，成功结果已经确认提交。
	refreshSession func(context.Context, string) (*domain.RefreshSessionResult, error)
	// issueAccessToken 绑定已有 AccessTokenSigner.Issue。
	issueAccessToken func(domain.LoginSession) (*domain.AccessToken, error)
}

// NewRefreshTokenService 保存刷新会话和令牌签发方法，缺少依赖属于装配错误。
func NewRefreshTokenService(
	refreshSession func(context.Context, string) (*domain.RefreshSessionResult, error),
	issueAccessToken func(domain.LoginSession) (*domain.AccessToken, error),
) *RefreshTokenService {
	// 1. 检查并保存本层需要的两个已有能力。
	if refreshSession == nil || issueAccessToken == nil {
		panic("刷新凭证服务需要刷新会话与访问令牌签发方法")
	}
	return &RefreshTokenService{refreshSession: refreshSession, issueAccessToken: issueAccessToken}
}

// Refresh 轮换刷新凭证并在提交后签发访问令牌，错误时返回空结果。
// 已确认提交的轮换在后续签发或交付失败时仍然生效。
func (tokenService *RefreshTokenService) Refresh(ctx context.Context, rawToken string) (*domain.RefreshTokenResult, error) {
	// 1. 检查请求上下文。
	if ctx == nil {
		return nil, fmt.Errorf("刷新凭证需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 取得已提交的刷新会话，检查结果完整性。
	sessionResult, sessionError := tokenService.refreshSession(ctx, rawToken)
	if sessionError != nil {
		return nil, fmt.Errorf("刷新凭证会话失败: %w", sessionError)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sessionResult == nil {
		return nil, domain.ErrInvalidSessionData
	}
	refreshed := *sessionResult
	if refreshed.Session.ID == 0 || refreshed.Session.UserID == 0 || refreshed.Session.DeviceID == 0 ||
		refreshed.Session.ExpiresAt.IsZero() || refreshed.TokenGeneration < 2 || refreshed.RefreshToken == "" {
		return nil, domain.ErrInvalidSessionData
	}

	// 3. 使用已提交会话签发 Access Token。
	accessToken, tokenError := tokenService.issueAccessToken(refreshed.Session)
	if tokenError != nil {
		return nil, fmt.Errorf("签发刷新访问令牌失败: %w", tokenError)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accessToken == nil || accessToken.Token == "" || accessToken.ExpiresAt.IsZero() ||
		accessToken.ExpiresAt.After(refreshed.Session.ExpiresAt) {
		return nil, fmt.Errorf("刷新访问令牌签发结果异常")
	}

	// 4. 返回包含两类新凭证的刷新结果。
	return &domain.RefreshTokenResult{
		Session:         refreshed.Session,
		TokenGeneration: refreshed.TokenGeneration,
		AccessToken:     *accessToken,
		RefreshToken:    refreshed.RefreshToken,
	}, nil
}
