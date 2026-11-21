package service

import (
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const maxUploadPartMismatchNumbers = 20

// MatchUploadCompletionParts 对照固定计划和完整存储回执核对客户端的完成声明。
// 成功交付可用于完成快照的独立回执。发生任何失配或错误时不交付部分结果。
func MatchUploadCompletionParts(
	fileSizeBytes, partSizeBytes uint64,
	partCount uint32,
	claims []domain.UploadPartCompletionClaim,
	receipts []domain.UploadPartReceipt,
) ([]domain.UploadPartReceipt, error) {
	// 1. 复用已保存计划的片长规则，校验存储回执。
	available, err := FilterUploadPartReceipts(fileSizeBytes, partSizeBytes, partCount, receipts)
	if err != nil {
		return nil, err
	}
	for _, receipt := range receipts {
		if !domain.ValidUploadPartETag(receipt.ETag) || !domain.ValidUploadPartChecksumSHA256(receipt.ChecksumSHA256) {
			return nil, domain.ErrUploadStorageUnavailable
		}
	}

	// 2. 按编号记录客户端声明，拒绝越界或重复。
	if len(claims) < 1 || len(claims) > domain.MaxUploadPartCount {
		return nil, &domain.UploadPartDeclarationError{Field: "parts", Message: fmt.Sprintf("完成回执须包含 1～%d 项", domain.MaxUploadPartCount)}
	}
	claimedETags := make(map[uint32]string, len(claims))
	for index, claim := range claims {
		if claim.PartNumber == 0 || claim.PartNumber > partCount {
			return nil, &domain.UploadPartDeclarationError{Field: fmt.Sprintf("parts.%d.part_number", index),
				Message: fmt.Sprintf("分片编号须为当前任务的 1～%d", partCount)}
		}
		if _, duplicate := claimedETags[claim.PartNumber]; duplicate {
			return nil, &domain.UploadPartDeclarationError{Field: "parts", Message: "完成回执的分片编号不得重复"}
		}
		if !domain.ValidUploadPartETag(claim.ETag) {
			return nil, &domain.UploadPartDeclarationError{Field: fmt.Sprintf("parts.%d.etag", index), Message: "分片 ETag 格式无效"}
		}
		claimedETags[claim.PartNumber] = claim.ETag
	}

	// 3. 按计划顺序逐片核对，交付完整回执或有界的失配编号。
	// 每个计划编号的实际长度均正确时，长度总和就等于声明的文件大小。
	var mismatchNumbers []uint32
	receiptIndex := 0
	for partNumber := uint32(1); partNumber <= partCount; partNumber++ {
		if receiptIndex < len(available) && available[receiptIndex].PartNumber == partNumber {
			receipt := available[receiptIndex]
			receiptIndex++
			if claimedETags[partNumber] == receipt.ETag {
				continue
			}
		}
		mismatchNumbers = append(mismatchNumbers, partNumber)
		if len(mismatchNumbers) == maxUploadPartMismatchNumbers {
			break
		}
	}
	if len(mismatchNumbers) > 0 {
		return nil, &domain.UploadPartsMismatchError{PartNumbers: mismatchNumbers}
	}
	return available, nil
}
