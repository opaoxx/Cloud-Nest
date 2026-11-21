package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// minUploadPartsResponsePartSize 是回执查询契约允许公开的最小标准片长，5 MiB。
const minUploadPartsResponsePartSize uint64 = 5 << 20

// UploadPartReceiptData 保存客户端用于续传比对的分片回执。
type UploadPartReceiptData struct {
	PartNumber     uint32 `json:"part_number"`
	SizeBytes      uint64 `json:"size_bytes"`
	ETag           string `json:"etag"`
	ChecksumSHA256 string `json:"checksum_sha256"`
}

// UploadPartsData 保存任务的固定分片计划及本轮核对出的可用回执。
type UploadPartsData struct {
	PartSizeBytes uint64                  `json:"part_size_bytes"`
	PartCount     uint32                  `json:"part_count"`
	Parts         []UploadPartReceiptData `json:"parts"`
}

// UploadPartsResponse 关联分片回执清单与当前请求 ID。
type UploadPartsResponse struct {
	Data      UploadPartsData `json:"data"`
	RequestID string          `json:"request_id"`
}

// NewUploadPartsData 将已经核对的回执转换成公开字段，任一项异常时拒绝整批。
func NewUploadPartsData(result *domain.UploadPartReceiptList) (*UploadPartsData, error) {
	// 1. 校验标准片长和总片数，确保可转换为客户端可精确表示的整数。
	if result == nil || result.PartSizeBytes < minUploadPartsResponsePartSize || result.PartSizeBytes > maxJSONSafeInteger ||
		result.PartCount < 1 || result.PartCount > domain.MaxUploadPartCount || len(result.Parts) > int(result.PartCount) {
		return nil, domain.ErrInvalidUploadPartReceiptListData
	}

	// 2. 按编号升序核对回执，允许跳号。尾片可以短于标准片。
	parts := make([]UploadPartReceiptData, 0, len(result.Parts))
	var previousNumber uint32
	for _, receipt := range result.Parts {
		if receipt.PartNumber <= previousNumber || receipt.PartNumber > result.PartCount ||
			receipt.SizeBytes == 0 || receipt.SizeBytes > result.PartSizeBytes ||
			(receipt.PartNumber < result.PartCount && receipt.SizeBytes != result.PartSizeBytes) ||
			!domain.ValidUploadPartETag(receipt.ETag) || !domain.ValidUploadPartChecksumSHA256(receipt.ChecksumSHA256) {
			return nil, domain.ErrInvalidUploadPartReceiptListData
		}
		previousNumber = receipt.PartNumber

		// 3. 复制公开字段，保留 ETag 和摘要原值。没有可用回执时 parts 编码为 []。
		parts = append(parts, UploadPartReceiptData{
			PartNumber: receipt.PartNumber, SizeBytes: receipt.SizeBytes,
			ETag: receipt.ETag, ChecksumSHA256: receipt.ChecksumSHA256,
		})
	}
	return &UploadPartsData{PartSizeBytes: result.PartSizeBytes, PartCount: result.PartCount, Parts: parts}, nil
}
