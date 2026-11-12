package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WithUserTransaction 锁定并复查已认证 Actor 的当前数据库状态，再执行写事务回调。
// actor 须来自认证流程；回调仅使用传入上下文与 Store，返回后不得继续持有事务。
// 用户锁、最多 5 秒期限、提交/回滚及禁止嵌套规则复用已有用户事务。
func (store *Store) WithUserTransaction(ctx context.Context, actor domain.Actor, action func(context.Context, *Store) error) error {
	// 1. 检查 Actor 与回调，进入已有用户事务。
	if ctx == nil || action == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return fmt.Errorf("已认证用户事务需要上下文、完整 Actor 和回调")
	}
	return store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
		// 2. 按用户、设备、会话顺序锁定当前记录。
		var device models.LoginDevice
		queryError := txStore.database.WithContext(txContext).
			Select("id", "user_id", "revoked_at").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND id = ?", actor.UserID, actor.DeviceID).Take(&device).Error
		if err := txContext.Err(); err != nil {
			return err
		}
		if errors.Is(queryError, gorm.ErrRecordNotFound) {
			return domain.ErrSessionNotFound
		}
		if queryError != nil {
			return fmt.Errorf("锁定已认证用户设备失败: %w", queryError)
		}
		if device.ID != actor.DeviceID || device.UserID != actor.UserID {
			return domain.ErrInvalidDeviceData
		}
		var session models.LockedSession
		queryError = txStore.database.WithContext(txContext).
			Select("id", "user_id", "device_id", "expires_at", "revoked_at").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND device_id = ? AND id = ?", actor.UserID, actor.DeviceID, actor.SessionID).
			Take(&session).Error
		if err := txContext.Err(); err != nil {
			return err
		}
		if errors.Is(queryError, gorm.ErrRecordNotFound) {
			return domain.ErrSessionNotFound
		}
		if queryError != nil {
			return fmt.Errorf("锁定已认证用户会话失败: %w", queryError)
		}

		// 3. 复查归属、撤销状态和会话期限。
		if session.ID != actor.SessionID || session.UserID != actor.UserID || session.DeviceID != actor.DeviceID || session.ExpiresAt.IsZero() {
			return domain.ErrInvalidSessionData
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

		// 4. 执行同一事务中的回调，交由外层提交或回滚。
		authenticatedStore := &Store{
			database:           txStore.database,
			loginUserID:        txStore.loginUserID,
			authenticatedActor: &actor,
		}
		return action(txContext, authenticatedStore)
	})
}
