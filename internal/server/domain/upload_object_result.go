package domain

import "errors"

// ErrUploadObjectExists 表示条件创建发现该暂存对象键已经存在。
var ErrUploadObjectExists = errors.New("上传暂存对象已存在")

// ErrUploadStorageUnavailable 表示本次存储写入未能取得有效确认。
var ErrUploadStorageUnavailable = errors.New("存储服务暂时不可用")

// UploadObjectResult 保存本次暂存对象写入的确认信息。
type UploadObjectResult struct {
	// SizeBytes 是已核对临时文件大小后提交的字节数。
	SizeBytes uint64
	// StorageETag 是存储返回的不透明 ETag，缺失时为 nil。
	StorageETag *string
	// StorageVersionID 是存储返回的版本定位，缺失时为 nil。
	StorageVersionID *string
}
