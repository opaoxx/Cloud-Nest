package domain

import (
	"crypto/sha256"
	"errors"
)

// ErrUploadBodySizeMismatch 表示实际文件字节数与任务声明不一致。
var ErrUploadBodySizeMismatch = errors.New("文件实际大小与声明不一致")

// ErrUploadChecksumMismatch 表示完整文件摘要与客户端声明不一致。
var ErrUploadChecksumMismatch = errors.New("文件校验失败，请重新上传")

// UploadContentDigest 保存已完整读取并核对大小的文件事实。
type UploadContentDigest struct {
	// SizeBytes 是本次完整内容的实际字节数。
	SizeBytes uint64
	// SHA256 是本次完整内容的二进制摘要。
	SHA256 [sha256.Size]byte
}
