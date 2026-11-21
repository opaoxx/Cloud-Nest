package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-sql-driver/mysql"
)

const (
	mysqlLockWaitTimeout = 1205
	mysqlLockDeadlock    = 1213
)

// markUploadTransactionRetryError 为确认可重试的上传事务错误附加对应的领域标记。
func markUploadTransactionRetryError(ctx context.Context, err, retryMarker error) error {
	// 1. 确认调用仍有效，保留原来的取消、超时和其他错误。
	if err == nil || ctx == nil || ctx.Err() != nil {
		return err
	}
	// 2. 沿单条包装链找到 MySQL 错误，核对错误码。
	// 用户事务可能把回滚失败合并进原错误。多原因错误保留原样，留给调用方检查。
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		databaseError, ok := cause.(*mysql.MySQLError)
		if !ok || databaseError == nil {
			continue
		}
		switch databaseError.Number {
		case mysqlLockWaitTimeout, mysqlLockDeadlock:
			// 3. 给确认可重试的错误附加标记，同时保留原始原因。
			return fmt.Errorf("%w: %w", retryMarker, err)
		}
	}
	return err
}
