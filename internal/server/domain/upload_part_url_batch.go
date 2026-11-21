package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"
)

// ErrInvalidUploadPartURLBatchData 表示签发结果无法安全地作为完整批次交付。
var ErrInvalidUploadPartURLBatchData = errors.New("分片地址签发结果异常")

// UploadPartURL 保存客户端上传一片所需的签名和校验参数。
type UploadPartURL struct {
	PartNumber     uint32
	SizeBytes      uint64
	ChecksumSHA256 string
	URL            string
	URLExpiresAt   time.Time
}

// UploadPartURLBatch 保存当前任务的期限和按请求顺序签发的链接。
type UploadPartURLBatch struct {
	ExpiresAt time.Time
	Parts     []UploadPartURL
}

// ValidUploadPartChecksumSHA256 检查 32 字节摘要的标准 Base64 编码和结尾填充。
func ValidUploadPartChecksumSHA256(checksum string) bool {
	// 1. 先限制编码长度，再验证字符、填充位和解码长度。
	if len(checksum) != base64.StdEncoding.EncodedLen(sha256.Size) {
		return false
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(checksum)
	return err == nil && len(digest) == sha256.Size
}
