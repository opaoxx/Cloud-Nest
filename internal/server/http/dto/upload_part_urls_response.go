package dto

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadPartURLRequiredHeaders 保存客户端 PUT 时显式发送的存储请求头。
type UploadPartURLRequiredHeaders struct {
	ChecksumSHA256 string `json:"x-amz-checksum-sha256"`
}

// UploadPartURLData 保存一片的上传地址及签名所需的公开字段。
type UploadPartURLData struct {
	PartNumber      uint32                       `json:"part_number"`
	SizeBytes       uint64                       `json:"size_bytes"`
	URL             string                       `json:"url"`
	RequiredHeaders UploadPartURLRequiredHeaders `json:"required_headers"`
	URLExpiresAt    time.Time                    `json:"url_expires_at"`
}

// UploadPartURLsData 保存任务截止时间和按请求顺序签发的分片。
type UploadPartURLsData struct {
	ExpiresAt time.Time           `json:"expires_at"`
	Parts     []UploadPartURLData `json:"parts"`
}

// UploadPartURLsResponse 关联公开签发结果与本次请求 ID。
type UploadPartURLsResponse struct {
	Data      UploadPartURLsData `json:"data"`
	RequestID string             `json:"request_id"`
}

// NewUploadPartURLsData 转换已签发的整批结果，任何一项异常都会拒绝整个响应数据。
func NewUploadPartURLsData(result *domain.UploadPartURLBatch) (*UploadPartURLsData, error) {
	// 1. 检查批次数量，取得任务的 UTC 截止时间。
	if result == nil || len(result.Parts) < 1 || len(result.Parts) > domain.MaxUploadPartSigningInputs {
		return nil, domain.ErrInvalidUploadPartURLBatchData
	}
	taskExpiry, valid := listResponseTime(&result.ExpiresAt)
	if !valid {
		return nil, domain.ErrInvalidUploadPartURLBatchData
	}

	// 2. 逐片核对长度、签名地址、摘要和有效期，拒绝重复编号。
	parts := make([]UploadPartURLData, 0, len(result.Parts))
	seen := make(map[uint32]struct{}, len(result.Parts))
	for _, part := range result.Parts {
		link := domain.SignedUploadPartURL{URL: part.URL, ExpiresAt: part.URLExpiresAt}
		linkExpiry, valid := listResponseTime(&part.URLExpiresAt)
		if part.PartNumber < 1 || part.PartNumber > domain.MaxUploadPartCount ||
			part.SizeBytes < 1 || part.SizeBytes > maxJSONSafeInteger ||
			!link.IsValid() || !valid || part.URLExpiresAt.After(result.ExpiresAt) ||
			!domain.ValidUploadPartChecksumSHA256(part.ChecksumSHA256) {
			return nil, domain.ErrInvalidUploadPartURLBatchData
		}
		if _, duplicate := seen[part.PartNumber]; duplicate {
			return nil, domain.ErrInvalidUploadPartURLBatchData
		}
		seen[part.PartNumber] = struct{}{}

		// 3. 按原顺序组装公开字段，完整保留 URL 和已签名的摘要头。
		parts = append(parts, UploadPartURLData{
			PartNumber:      part.PartNumber,
			SizeBytes:       part.SizeBytes,
			URL:             part.URL,
			RequiredHeaders: UploadPartURLRequiredHeaders{ChecksumSHA256: part.ChecksumSHA256},
			URLExpiresAt:    *linkExpiry,
		})
	}
	return &UploadPartURLsData{ExpiresAt: *taskExpiry, Parts: parts}, nil
}
