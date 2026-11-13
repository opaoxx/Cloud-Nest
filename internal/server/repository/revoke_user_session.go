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

// RevokeUserSession 在已认证用户事务中撤销指定会话，归属取自事务绑定的 Actor。
// 已撤销的目标返回成功并保留原时间，不存在或其他账号的目标返回 ErrSessionNotFound。
// ctx 与 Store 来自 WithUserTransaction，错误由回调返回外层，提交成功后撤销确认生效。
func (store *Store) RevokeUserSession(ctx context.Context, targetSessionID uint64) error {
	// 1. 检查已认证事务、目标会话 ID 和上下文。
	if ctx == nil || targetSessionID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return fmt.Errorf("撤销指定会话需要上下文、非零目标 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("撤销指定会话必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	updateContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := updateContext.Err(); err != nil {
		return err
	}

	// 2. 先只读定位归属，再锁定并复查目标会话。
	// 普通读取可直接拒绝其他账号的 ID，避免等待该账号正在持有的会话锁。
	if _, err := store.FindSessionAuthorization(updateContext, actor.UserID, targetSessionID); err != nil {
		return err
	}
	// 外层已持有用户锁，同一用户的认证写入按该锁串行；此处继续锁定会话。
	var target models.LockedSession
	queryError := store.database.WithContext(updateContext).
		Select("id", "user_id", "device_id", "expires_at", "revoked_at").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND id = ?", actor.UserID, targetSessionID).
		Take(&target).Error
	if err := updateContext.Err(); err != nil {
		return err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return domain.ErrSessionNotFound
	}
	if queryError != nil {
		return fmt.Errorf("锁定待撤销会话失败: %w", queryError)
	}
	if target.ID != targetSessionID || target.UserID != actor.UserID || target.DeviceID == 0 || target.ExpiresAt.IsZero() {
		return domain.ErrInvalidSessionData
	}

	// 3. 已撤销时返回成功，否则写入数据库 UTC 撤销时间。
	if target.RevokedAt != nil {
		return nil
	}
	result := store.database.WithContext(updateContext).Exec(
		"UPDATE auth_sessions SET revoked_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND device_id = ? AND id = ? AND revoked_at IS NULL",
		actor.UserID, target.DeviceID, targetSessionID)

	// 4. 检查更新结果，交由外层事务提交或回滚。
	if err := updateContext.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("撤销指定会话失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidSessionData
	}
	return nil
}
