package repository

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WithRefreshUserTransaction 按用户、设备、会话顺序加锁，并复查初次校验结果。
// action 仅使用传入的事务上下文、Store 与当前资料，返回后不得继续持有事务。
// 事务期限、提交、回滚及禁止嵌套的规则复用 WithLoginUserTransaction。
func (store *Store) WithRefreshUserTransaction(
	ctx context.Context,
	verified domain.VerifiedRefreshSession,
	action func(context.Context, *Store, domain.VerifiedRefreshSession) error,
) error {
	// 1. 检查校验结果和回调，进入已有用户事务。
	if ctx == nil || action == nil || verified.Session.UserID == 0 || verified.Session.ID == 0 ||
		verified.Session.DeviceID == 0 || verified.TokenGeneration == 0 {
		return fmt.Errorf("刷新用户事务需要上下文、完整校验标识、非零代次和回调")
	}
	return store.WithLoginUserTransaction(ctx, verified.Session.UserID, func(txContext context.Context, txStore *Store) error {
		// 2. 按归属锁定设备，再锁定会话。
		var device models.LoginDevice
		queryError := txStore.database.WithContext(txContext).
			Select("id", "user_id", "revoked_at").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND id = ?", verified.Session.UserID, verified.Session.DeviceID).Take(&device).Error
		if err := txContext.Err(); err != nil {
			return err
		}
		if errors.Is(queryError, gorm.ErrRecordNotFound) {
			return domain.ErrDeviceNotFound
		}
		if queryError != nil {
			return fmt.Errorf("锁定刷新设备失败: %w", queryError)
		}
		if device.ID != verified.Session.DeviceID || device.UserID != verified.Session.UserID {
			return domain.ErrInvalidDeviceData
		}
		var session models.LockedRefreshSession
		queryError = txStore.database.WithContext(txContext).
			Select("id", "user_id", "device_id", "refresh_token_hash", "token_generation", "expires_at", "revoked_at").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND device_id = ? AND id = ?", verified.Session.UserID, device.ID, verified.Session.ID).
			Take(&session).Error
		if err := txContext.Err(); err != nil {
			return err
		}
		if errors.Is(queryError, gorm.ErrRecordNotFound) {
			return domain.ErrInvalidRefreshToken
		}
		if queryError != nil {
			return fmt.Errorf("锁定刷新会话失败: %w", queryError)
		}

		// 3. 复查当前摘要、代次、撤销状态和到期时间。
		if session.ID != verified.Session.ID || session.UserID != verified.Session.UserID ||
			session.DeviceID != device.ID || session.TokenGeneration == 0 ||
			len(session.RefreshTokenHash) != sha256.Size || session.ExpiresAt.IsZero() {
			return domain.ErrInvalidSessionData
		}
		if session.TokenGeneration != verified.TokenGeneration ||
			subtle.ConstantTimeCompare(session.RefreshTokenHash, verified.TokenHash[:]) != 1 {
			return domain.ErrInvalidRefreshToken
		}
		if session.RevokedAt != nil {
			return domain.ErrSessionRevoked
		}
		if device.RevokedAt != nil {
			return domain.ErrDeviceRevoked
		}
		if !session.ExpiresAt.After(time.Now()) {
			return domain.ErrSessionExpired
		}
		if err := txContext.Err(); err != nil {
			return err
		}

		// 4. 将锁内最新资料交给同一事务中的回调。
		current := domain.VerifiedRefreshSession{
			Session: domain.LoginSession{
				ID: session.ID, UserID: session.UserID, DeviceID: session.DeviceID,
				ExpiresAt: session.ExpiresAt.UTC(),
			},
			TokenGeneration: session.TokenGeneration,
			TokenHash:       verified.TokenHash,
		}
		refreshStore := &Store{
			database:       txStore.database,
			loginUserID:    txStore.loginUserID,
			refreshSession: &current,
		}
		return action(txContext, refreshStore, current)
	})
}
