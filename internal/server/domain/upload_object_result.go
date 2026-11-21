package domain

import "errors"

// ErrUploadObjectExists 表示条件创建发现该暂存对象键已经存在。
var ErrUploadObjectExists = errors.New("上传暂存对象已存在")

// ErrUploadObjectMissing 表示校验所需的物理对象或指定版本不存在。
var ErrUploadObjectMissing = errors.New("待校验的上传对象不存在")

// ErrUploadStorageUnavailable 表示本次对象存储读写未能完成。
var ErrUploadStorageUnavailable = errors.New("存储服务暂时不可用")

// UploadObjectResult 保存从存储写入确认或元数据查询取得的对象信息。
type UploadObjectResult struct {
	// SizeBytes 是已提交或从存储查询到的对象字节数。
	SizeBytes uint64
	// StorageETag 是存储返回的不透明 ETag，缺失时为 nil。
	StorageETag *string
	// StorageVersionID 是存储返回的版本定位，缺失时为 nil。
	StorageVersionID *string
}
