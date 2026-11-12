package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// RevokeCurrentSession 在已认证用户事务中撤销当前绑定的会话。
// ctx 与 Store 来自 WithUserTransaction，错误应由回调返回外层，提交成功后撤销确认生效。
func (store *Store) RevokeCurrentSession(ctx context.Context) error {
	// 1. 确认已认证事务和请求上下文有效。
	if ctx == nil || store.authenticatedActor == nil || store.loginUserID == 0 {
		return fmt.Errorf("撤销当前会话需要上下文和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("撤销当前会话必须使用已认证事务 Store")
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

	// 2. 按当前身份更新会话撤销时间。
	result := store.database.WithContext(updateContext).Exec(
		"UPDATE auth_sessions SET revoked_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND device_id = ? AND id = ? AND revoked_at IS NULL",
		actor.UserID, actor.DeviceID, actor.SessionID)

	// 3. 检查取消、数据库错误和更新行数。
	if err := updateContext.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("撤销当前会话失败: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return domain.ErrSessionRevoked
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidSessionData
	}

	// 4. 返回执行结果，等待外层事务提交。
	return nil
}
