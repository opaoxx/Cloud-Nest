package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RefreshSessionTransaction 列出刷新服务在已复查的用户事务中需要的写入能力。
type RefreshSessionTransaction interface {
	// RotateRefreshToken 按当前绑定的旧摘要和代次轮换，结果等待外层事务提交。
	RotateRefreshToken(ctx context.Context, newHash [sha256.Size]byte) (*domain.VerifiedRefreshSession, error)
}

// RefreshSessionService 组合旧原文校验、新凭证生成和事务轮换。
type RefreshSessionService struct {
	// verifyRefreshToken 绑定刷新凭证初次校验。
	verifyRefreshToken func(context.Context, string) (*domain.VerifiedRefreshSession, error)
	// withTransaction 同步执行一次回调，保留回调错误，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.VerifiedRefreshSession, func(context.Context, RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error
	// generateRefreshToken 生成提交后交付的新原文及其文本摘要。
	generateRefreshToken func() (string, [sha256.Size]byte)
	// now 用于复查依赖返回的会话有效期。
	now func() time.Time
}

// NewRefreshSessionService 绑定校验和刷新事务入口，缺少依赖属于装配错误。
func NewRefreshSessionService(
	verifyRefreshToken func(context.Context, string) (*domain.VerifiedRefreshSession, error),
	withTransaction func(context.Context, domain.VerifiedRefreshSession, func(context.Context, RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error,
) *RefreshSessionService {
	// 1. 检查服务装配所需的两个依赖。
	if verifyRefreshToken == nil || withTransaction == nil {
		panic("刷新会话服务需要凭证校验和刷新事务方法")
	}
	// 2. 保存依赖并绑定已有生成器与当前时钟。
	return &RefreshSessionService{
		verifyRefreshToken:   verifyRefreshToken,
		withTransaction:      withTransaction,
		generateRefreshToken: GenerateRefreshToken,
		now:                  time.Now,
	}
}

// Refresh 轮换有效刷新凭证，事务错误时返回空结果并保留原因。
// 提交故障结束本次调用；事务确认提交后返回已提交结果，后续交付层继续处理取消。
func (sessionService *RefreshSessionService) Refresh(ctx context.Context, rawToken string) (*domain.RefreshSessionResult, error) {
	// 1. 校验旧刷新原文，取得初次会话资料。
	if ctx == nil {
		return nil, fmt.Errorf("刷新会话需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, verifyError := sessionService.verifyRefreshToken(ctx, rawToken)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verifyError != nil {
		return nil, fmt.Errorf("校验刷新会话凭证失败: %w", verifyError)
	}
	if snapshot == nil || snapshot.Session.ID == 0 || snapshot.Session.UserID == 0 || snapshot.Session.DeviceID == 0 ||
		snapshot.Session.ExpiresAt.IsZero() || snapshot.TokenGeneration == 0 || snapshot.TokenGeneration == ^uint64(0) ||
		snapshot.TokenHash != sha256.Sum256([]byte(rawToken)) {
		return nil, domain.ErrInvalidSessionData
	}
	verified := *snapshot
	if !verified.Session.ExpiresAt.After(sessionService.now()) {
		return nil, domain.ErrSessionExpired
	}

	// 2. 生成新刷新原文与摘要，准备事务输入。
	newRawToken, newHash := sessionService.generateRefreshToken()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 3. 在刷新用户事务中轮换摘要，检查并暂存结果。
	var prepared *domain.RefreshSessionResult
	transactionError := sessionService.withTransaction(ctx, verified, func(txContext context.Context, txStore RefreshSessionTransaction, current domain.VerifiedRefreshSession) error {
		// 1. 检查事务上下文及锁内资料与本次旧凭证的归属。
		if txContext == nil || txStore == nil {
			return fmt.Errorf("刷新会话回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		if current.Session.ID != verified.Session.ID || current.Session.UserID != verified.Session.UserID ||
			current.Session.DeviceID != verified.Session.DeviceID || current.Session.ExpiresAt.IsZero() ||
			current.TokenGeneration != verified.TokenGeneration || current.TokenHash != verified.TokenHash {
			return domain.ErrInvalidSessionData
		}
		if !current.Session.ExpiresAt.After(sessionService.now()) {
			return domain.ErrSessionExpired
		}

		// 2. 使用同一事务 Store 写入新摘要，核对返回的归属、期限及下一代次。
		rotated, rotateError := txStore.RotateRefreshToken(txContext, newHash)
		if err := txContext.Err(); err != nil {
			return err
		}
		if rotateError != nil {
			return fmt.Errorf("轮换刷新会话失败: %w", rotateError)
		}
		if rotated == nil || rotated.Session.ID != current.Session.ID || rotated.Session.UserID != current.Session.UserID ||
			rotated.Session.DeviceID != current.Session.DeviceID || !rotated.Session.ExpiresAt.Equal(current.Session.ExpiresAt) ||
			rotated.TokenGeneration != current.TokenGeneration+1 || rotated.TokenHash != newHash {
			return domain.ErrInvalidSessionData
		}

		// 3. 保存结果副本，等待刷新用户事务确认提交。
		preparedSession := rotated.Session
		preparedSession.ExpiresAt = preparedSession.ExpiresAt.UTC()
		prepared = &domain.RefreshSessionResult{
			Session: preparedSession, TokenGeneration: rotated.TokenGeneration, RefreshToken: newRawToken,
		}
		return nil
	})

	// 4. 确认外层事务提交成功，返回新刷新凭证。
	if transactionError != nil {
		return nil, fmt.Errorf("刷新会话事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidSessionData
	}
	return prepared, nil
}
