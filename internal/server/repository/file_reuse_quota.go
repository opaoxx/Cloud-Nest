package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// CheckUserFileReuseQuota 在 WithUserTransaction 回调内检查新逻辑文件是否有足够配额。
// 校验使用当前已用及预占，写入与已用空间结算由后续事务步骤负责。
func (store *Store) CheckUserFileReuseQuota(ctx context.Context, sizeBytes uint64) error {
	// 1. 核对已认证的账号事务和上下文。
	if ctx == nil || store == nil || store.database == nil || store.authenticatedActor == nil || store.loginUserID == 0 {
		return fmt.Errorf("检查秒传配额需要上下文和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("检查秒传配额必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return err
	}

	// 2. 读取锁内账目并复用剩余空间判断。
	if _, err := store.lockUserQuotaForAdditionalBytes(queryContext, actor.UserID, sizeBytes); err != nil {
		return err
	}

	// 3. 确认读取未被取消，交付只读结果。
	return queryContext.Err()
}
