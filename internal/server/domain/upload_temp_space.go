package domain

import "errors"

// ErrUploadTempSpaceExhausted 表示临时区剩余预算不足以接收本次声明大小。
var ErrUploadTempSpaceExhausted = errors.New("上传临时空间不足，请稍后重试")
