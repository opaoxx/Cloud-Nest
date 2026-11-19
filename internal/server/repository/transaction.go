package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WithLoginUserTransaction 在登录写入前锁定并复查用户，再执行同一事务中的 Repository 回调。
// userID 必须来自已验证凭证；action 只使用传入上下文及 Store，不做外部网络调用或自行提交。
// 不支持嵌套，最多 5 秒并服从调用方更短期限；回调和事务 Store 不得在返回后继续使用。
func (store *Store) WithLoginUserTransaction(
	ctx context.Context,
	userID uint64,
	action func(context.Context, *Store) error,
) error {
	return store.withActiveUserTransaction(ctx, userID, action)
}

// withActiveUserTransaction 供已验证凭证及可信内部任务共用，按用户串行化写入。
func (store *Store) withActiveUserTransaction(
	ctx context.Context,
	userID uint64,
	action func(context.Context, *Store) error,
) error {
	return store.withLockedUserTransaction(ctx, userID, false, action)
}

// withLockedUserTransaction 统一按用户加锁，内部清理可处理已禁用账号的遗留任务。
func (store *Store) withLockedUserTransaction(
	ctx context.Context,
	userID uint64,
	allowDisabled bool,
	action func(context.Context, *Store) error,
) (err error) {
	// 1. 检查参数，开启有期限的事务并登记回滚。
	if ctx == nil || userID == 0 || action == nil {
		return fmt.Errorf("用户事务需要上下文、非零用户 ID 和回调")
	}
	if _, nested := store.database.Statement.ConnPool.(gorm.TxCommitter); nested {
		return fmt.Errorf("用户事务不支持嵌套调用")
	}
	transactionContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := transactionContext.Err(); err != nil {
		return fmt.Errorf("开启用户事务失败: %w", err)
	}
	transaction := store.database.WithContext(transactionContext).Begin()
	if transaction.Error != nil {
		return fmt.Errorf("开启用户事务失败: %w", transaction.Error)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackError := transaction.Rollback().Error
			if rollbackError != nil && !errors.Is(rollbackError, sql.ErrTxDone) {
				err = errors.Join(err, fmt.Errorf("回滚用户事务失败: %w", rollbackError))
			}
		}
	}()

	// 2. 锁定用户行，重新检查账号状态。
	var account models.LoginAccount
	queryError := transaction.Select("id", "status").
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", userID).Take(&account).Error
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return domain.ErrAccountNotFound
	}
	if queryError != nil {
		return fmt.Errorf("锁定用户失败: %w", queryError)
	}
	if account.ID != userID {
		return domain.ErrInvalidAccountData
	}
	switch account.Status {
	case "ACTIVE":
	case "DISABLED":
		if !allowDisabled {
			return domain.ErrAccountDisabled
		}
	default:
		return domain.ErrInvalidAccountData
	}
	if err := transactionContext.Err(); err != nil {
		return fmt.Errorf("用户事务已取消: %w", err)
	}

	// 3. 调用事务内的 Repository 回调，失败时保留错误原因。
	if callbackError := action(transactionContext, &Store{database: transaction, loginUserID: userID}); callbackError != nil {
		return fmt.Errorf("执行用户事务失败: %w", callbackError)
	}

	// 4. 确认上下文仍有效，提交后才返回成功。
	if err := transactionContext.Err(); err != nil {
		return fmt.Errorf("提交用户事务前已取消: %w", err)
	}
	if commitError := transaction.Commit().Error; commitError != nil {
		return fmt.Errorf("提交用户事务失败: %w", commitError)
	}
	committed = true
	return nil
}
