package domain

import "errors"

// ErrUserTransactionCommitUnconfirmed 表示提交调用未返回成功，结果可能已被数据库接受。
var ErrUserTransactionCommitUnconfirmed = errors.New("用户事务提交结果未确认")
