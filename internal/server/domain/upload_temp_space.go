package domain

import "errors"

// ErrUploadTempSpaceExhausted 表示暂存预算不足或暂存文件写入时磁盘空间耗尽。
var ErrUploadTempSpaceExhausted = errors.New("上传临时空间不足，请稍后重试")

// ErrUploadReceiverBusy 表示共享临时区的三个接收名额均在使用。
var ErrUploadReceiverBusy = errors.New("上传接收繁忙，请稍后重试")
