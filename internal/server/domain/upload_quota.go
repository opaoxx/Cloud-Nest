package domain

import "errors"

// ErrQuotaExceeded 表示剩余逻辑配额不足以预占本次上传声明。
var ErrQuotaExceeded = errors.New("存储空间不足")
