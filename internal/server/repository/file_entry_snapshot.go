package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// WithFileEntrySnapshot 在同一只读可重复读事务中执行目录项读取。
// 首次普通查询建立快照，后续普通查询复用该快照，用户归属由查询方法限定。
// action 仅使用传入上下文与 Store 读取，返回后释放引用，提交与回滚由本入口管理。
// 事务最多五秒并服从调用方期限，嵌套调用返回错误。
func (store *Store) WithFileEntrySnapshot(ctx context.Context, action func(context.Context, *Store) error) (err error) {
	// 1. 检查上下文、回调及嵌套调用，设置事务期限。
	if ctx == nil || action == nil {
		return fmt.Errorf("目录项快照需要上下文和读取回调")
	}
	if _, nested := store.database.Statement.ConnPool.(gorm.TxCommitter); nested {
		return fmt.Errorf("目录项快照不支持嵌套事务")
	}
	transactionContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := transactionContext.Err(); err != nil {
		return fmt.Errorf("开启目录项快照失败: %w", err)
	}

	// 2. 开启只读可重复读事务，登记失败时的回滚。
	transaction := store.database.WithContext(transactionContext).Begin(&sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if transaction.Error != nil {
		return fmt.Errorf("开启目录项快照失败: %w", transaction.Error)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackError := transaction.Rollback().Error
			if rollbackError != nil && !errors.Is(rollbackError, sql.ErrTxDone) {
				err = errors.Join(err, fmt.Errorf("回滚目录项快照失败: %w", rollbackError))
			}
		}
	}()

	// 3. 使用同一事务上下文和 Store 执行读取回调。
	if callbackError := action(transactionContext, &Store{database: transaction}); callbackError != nil {
		return fmt.Errorf("执行目录项快照失败: %w", callbackError)
	}

	// 4. 复查取消，确认事务结束后返回成功。
	if err := transactionContext.Err(); err != nil {
		return fmt.Errorf("结束目录项快照前已取消: %w", err)
	}
	if commitError := transaction.Commit().Error; commitError != nil {
		return fmt.Errorf("结束目录项快照失败: %w", commitError)
	}
	committed = true
	return nil
}
