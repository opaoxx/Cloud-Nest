package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LockUserUploadTask 在 WithUserTransaction 回调中锁定当前用户的完整上传任务。
// 任务锁随外层事务提交或回滚释放，调用方须继续使用当前事务上下文与 Store。
func (store *Store) LockUserUploadTask(ctx context.Context, taskID uint64) (*domain.UploadTask, error) {
	// 1. 检查上下文、任务 ID 和已认证用户事务。
	if ctx == nil || taskID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("锁定上传任务需要上下文、非零任务 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("锁定上传任务必须使用已认证事务 Store")
	}

	// 2. 核对事务绑定的用户、设备及会话标识。
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}

	// 3. 在同一事务连接上附加 FOR UPDATE，复用完整任务读取与校验。
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	return lockedStore.FindUserUploadTask(ctx, actor.UserID, taskID)
}
