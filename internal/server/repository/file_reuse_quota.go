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
	if err := store.validateFileReuseQuotaTransaction(ctx); err != nil {
		return err
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return err
	}

	// 2. 读取锁内账目并复用剩余空间判断。
	if _, err := store.lockUserQuotaForAdditionalBytes(queryContext, store.loginUserID, sizeBytes); err != nil {
		return err
	}

	// 3. 确认读取未被取消，交付只读结果。
	return queryContext.Err()
}

// SettleUserFileReuseQuota 在 WithUserTransaction 回调内将新逻辑文件的完整大小计入已用空间。
// 新文件写入后调用一次，幂等重放由上层返回原结果，回调须传递本方法的错误。
func (store *Store) SettleUserFileReuseQuota(ctx context.Context, sizeBytes uint64) error {
	// 1. 核对当前调用来自已认证的账号事务。
	if err := store.validateFileReuseQuotaTransaction(ctx); err != nil {
		return err
	}
	updateContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// 2. 加锁重读配额，按已用空间和预占空间判断余额。
	account, err := store.lockUserQuotaForAdditionalBytes(updateContext, store.loginUserID, sizeBytes)
	if err != nil {
		return err
	}

	// 3. 按刚读到的账目更新已用空间，零字节跳过写入。
	if sizeBytes == 0 {
		return updateContext.Err()
	}
	// 上一步已验证有足够余额，因此已用空间加上新文件大小不会溢出。
	usedBytes := account.UsedBytes + sizeBytes
	result := store.database.WithContext(updateContext).Exec(
		"UPDATE users SET used_storage = ?, updated_at = UTC_TIMESTAMP(6) "+
			"WHERE id = ? AND status = 'ACTIVE' AND storage_quota = ? AND used_storage = ? AND reserved_storage = ?",
		usedBytes, account.ID, account.QuotaBytes, account.UsedBytes, account.ReservedBytes)

	// 4. 检查写入结果，把提交或回滚交给外层事务。
	if err := updateContext.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("结算秒传配额失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidAccountData
	}
	return nil
}

// validateFileReuseQuotaTransaction 让预检和结算采用同一账号范围。
func (store *Store) validateFileReuseQuotaTransaction(ctx context.Context) error {
	if ctx == nil || store == nil || store.database == nil || store.authenticatedActor == nil || store.loginUserID == 0 {
		return fmt.Errorf("秒传配额操作需要上下文和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("秒传配额操作必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	return ctx.Err()
}
