package service

import (
	"crypto/sha256"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateUploadContentChecksum 核对完整复制后的实际摘要与客户端可空的预期摘要。
// actual 由 CopyUploadContent 成功结果提供，expected 来自当前上传任务。
func ValidateUploadContentChecksum(actual [sha256.Size]byte, expected *[sha256.Size]byte) error {
	// 1. 客户端未声明预期摘要时通过。
	if expected == nil {
		return nil
	}

	// 2. 比较完整三十二字节摘要，不一致时返回固定领域错误。
	if actual != *expected {
		return domain.ErrUploadChecksumMismatch
	}

	// 3. 摘要一致时返回成功。
	return nil
}
