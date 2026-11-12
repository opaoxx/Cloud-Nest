package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RefreshCredentialService 校验刷新原文并检查其会话授权快照。
type RefreshCredentialService struct {
	// findSession 绑定 Repository.FindRefreshSession。
	findSession func(context.Context, [sha256.Size]byte) (*domain.RefreshSession, error)
	// now 在查询后检查会话截止时间，生产构造时绑定当前时钟。
	now func() time.Time
}

// NewRefreshCredentialService 保存摘要查询能力，缺少依赖属于装配错误。
func NewRefreshCredentialService(findSession func(context.Context, [sha256.Size]byte) (*domain.RefreshSession, error)) *RefreshCredentialService {
	// 1. 检查并保存依赖，绑定查询后的时间来源。
	if findSession == nil {
		panic("刷新凭证服务需要摘要会话查询方法")
	}
	return &RefreshCredentialService{findSession: findSession, now: time.Now}
}

// Verify 校验原文格式及当前读取状态，失败返回 nil。
// 成功结果用于定位后续事务，轮换写入仍须复查摘要、代次和授权状态。
func (refreshService *RefreshCredentialService) Verify(ctx context.Context, rawToken string) (*domain.VerifiedRefreshSession, error) {
	// 1. 检查上下文与刷新原文格式，计算文本摘要。
	if ctx == nil {
		return nil, fmt.Errorf("校验刷新凭证需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(rawToken) != base64.RawURLEncoding.EncodedLen(32) {
		return nil, domain.ErrInvalidRefreshToken
	}
	decoded, decodeError := base64.RawURLEncoding.Strict().DecodeString(rawToken)
	if decodeError != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != rawToken {
		return nil, domain.ErrInvalidRefreshToken
	}
	tokenHash := sha256.Sum256([]byte(rawToken))

	// 2. 按摘要读取刷新会话，处理取消与查询故障。
	snapshot, queryError := refreshService.findSession(ctx, tokenHash)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, domain.ErrSessionNotFound) {
		return nil, domain.ErrInvalidRefreshToken
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取刷新凭证会话失败: %w", queryError)
	}

	// 3. 检查标识、代次、账号及撤销和到期状态。
	if snapshot == nil || snapshot.UserID == 0 || snapshot.SessionID == 0 || snapshot.DeviceID == 0 ||
		snapshot.TokenGeneration == 0 || snapshot.SessionExpiresAt.IsZero() {
		return nil, domain.ErrInvalidSessionData
	}
	switch snapshot.UserStatus {
	case "ACTIVE":
	case "DISABLED":
		return nil, domain.ErrAccountDisabled
	default:
		return nil, domain.ErrInvalidAccountData
	}
	if snapshot.SessionRevokedAt != nil {
		return nil, domain.ErrSessionRevoked
	}
	if snapshot.DeviceRevokedAt != nil {
		return nil, domain.ErrDeviceRevoked
	}
	if !snapshot.SessionExpiresAt.After(refreshService.now()) {
		return nil, domain.ErrSessionExpired
	}

	// 4. 返回供后续事务复查使用的校验结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &domain.VerifiedRefreshSession{
		Session: domain.LoginSession{
			ID:        snapshot.SessionID,
			UserID:    snapshot.UserID,
			DeviceID:  snapshot.DeviceID,
			ExpiresAt: snapshot.SessionExpiresAt.UTC(),
		},
		TokenGeneration: snapshot.TokenGeneration,
		TokenHash:       tokenHash,
	}, nil
}
