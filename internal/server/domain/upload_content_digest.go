package domain

import (
	"crypto/sha256"
	"errors"
)

// ErrUploadBodySizeMismatch 表示实际文件字节数与任务声明不一致。
var ErrUploadBodySizeMismatch = errors.New("文件实际大小与声明不一致")

// UploadBodySizeMismatchCode 标记读回的文件大小与声明不符。
const UploadBodySizeMismatchCode = "BODY_SIZE_MISMATCH"

// UploadVerificationSizeMismatch 绑定本次读回的大小证据与校验尝试。
type UploadVerificationSizeMismatch struct {
	TaskID, UserID, StagedObjectID, LeaseVersion uint64
	// DeclaredSizeBytes 保存本次校验使用的声明长度。
	DeclaredSizeBytes uint64
	// ObservedSizeBytes 是提前读到 EOF 的长度，或超长时读到的声明长度加一。
	ObservedSizeBytes uint64
}

// Error 返回固定的大小不匹配提示。
func (*UploadVerificationSizeMismatch) Error() string { return ErrUploadBodySizeMismatch.Error() }

// Unwrap 支持已有大小不匹配的错误判断。
func (*UploadVerificationSizeMismatch) Unwrap() error { return ErrUploadBodySizeMismatch }

// ErrUploadChecksumMismatch 表示完整文件摘要与客户端声明不一致。
var ErrUploadChecksumMismatch = errors.New("文件校验失败，请重新上传")

// UploadChecksumMismatchCode 标记文件摘要与声明不符。
const UploadChecksumMismatchCode = "CHECKSUM_MISMATCH"

// UploadVerificationChecksumMismatch 绑定完整读回的实测摘要与本次校验尝试。
type UploadVerificationChecksumMismatch struct {
	TaskID, UserID, StagedObjectID, LeaseVersion uint64
	ExpectedSHA256                               [sha256.Size]byte
	Actual                                       UploadContentDigest
}

// Error 返回固定提示，保留摘要和内部标识供收尾校验使用。
func (*UploadVerificationChecksumMismatch) Error() string { return ErrUploadChecksumMismatch.Error() }

// Unwrap 保留已有摘要不匹配的错误判断。
func (*UploadVerificationChecksumMismatch) Unwrap() error { return ErrUploadChecksumMismatch }

// UploadContentDigest 保存已完整读取并核对大小的文件事实。
type UploadContentDigest struct {
	// SizeBytes 是本次完整内容的实际字节数。
	SizeBytes uint64
	// SHA256 是本次完整内容的二进制摘要。
	SHA256 [sha256.Size]byte
	// DetectedMIMEType 是对象校验读取时识别的基础媒体类型，空字符串表示未知。
	DetectedMIMEType string
}
