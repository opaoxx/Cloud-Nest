package service

import (
	"context"
	"errors"
	"slices"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadMergeReceiptSnapshot 保存本轮核对通过的目标和冻结回执，供后续合并使用。
type UploadMergeReceiptSnapshot struct {
	Target UploadPartSigningTarget
	Parts  []domain.UploadPartReceipt
}

// UploadMergeReceiptService 对照存储当前分片检查后台合并的冻结回执。
type UploadMergeReceiptService struct {
	getTarget  func(context.Context, *domain.UploadTask) (*UploadPartSigningTarget, error)
	readFrozen func(context.Context, *domain.UploadTask) ([]domain.UploadPartReceipt, error)
	listParts  func(context.Context, string, string) ([]domain.UploadPartReceipt, error)
}

// NewUploadMergeReceiptService 绑定持租约的目标查询、冻结回执读取及完整分页读取。
func NewUploadMergeReceiptService(
	getTarget func(context.Context, *domain.UploadTask) (*UploadPartSigningTarget, error),
	readFrozen func(context.Context, *domain.UploadTask) ([]domain.UploadPartReceipt, error),
	listParts func(context.Context, string, string) ([]domain.UploadPartReceipt, error),
) *UploadMergeReceiptService {
	if getTarget == nil || readFrozen == nil || listParts == nil {
		panic("后台合并核对需要目标查询、冻结回执和完整分页读取方法")
	}
	return &UploadMergeReceiptService{getTarget: getTarget, readFrozen: readFrozen, listParts: listParts}
}

// Verify 只读核对当前合并租约的目标与回执。会话缺失时保留信号，供调用方继续确认对象状态。
func (receiptService *UploadMergeReceiptService) Verify(ctx context.Context, claimed *domain.UploadTask) (*UploadMergeReceiptSnapshot, error) {
	// 1. 复核租约和存储目标，读取并检查冻结回执。
	target, err := loadUploadMergeTarget(ctx, claimed, receiptService.getTarget)
	if err != nil {
		return nil, err
	}
	return receiptService.verifyTarget(ctx, claimed, target)
}

// verifyTarget 对照已固定的原目标读取冻结回执和存储分片。
func (receiptService *UploadMergeReceiptService) verifyTarget(ctx context.Context, claimed *domain.UploadTask, target UploadPartSigningTarget) (*UploadMergeReceiptSnapshot, error) {
	// 1. 读取并检查同一租约的冻结回执。
	frozen, err := receiptService.readFrozen(ctx, claimed)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if !validFrozenMergeReceipts(target, frozen) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	frozen = slices.Clone(frozen)

	// 2. 只读获取完整存储清单，逐片比对长度、ETag 和 SHA-256。
	observed, listError := receiptService.listParts(ctx, target.ObjectKey, target.UploadID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if listError != nil && !errors.Is(listError, domain.ErrUploadPartSessionMissing) {
		return nil, domain.ErrUploadStorageUnavailable
	}
	var matchError error
	if listError == nil {
		matchError = compareUploadMergeReceipts(target, frozen, observed)
	}

	// 3. 再次复核租约和目标，返回完整结果或相应错误。
	if err := recheckUploadMergeTarget(ctx, claimed, target, receiptService.getTarget); err != nil {
		return nil, err
	}
	if listError != nil {
		return nil, domain.ErrUploadPartSessionMissing
	}
	if matchError != nil {
		return nil, matchError
	}
	return &UploadMergeReceiptSnapshot{Target: target, Parts: frozen}, nil
}

// validFrozenMergeReceipts 检查从仓储交付的快照确实覆盖固定计划。
func validFrozenMergeReceipts(target UploadPartSigningTarget, frozen []domain.UploadPartReceipt) bool {
	if len(frozen) != int(target.PartCount) {
		return false
	}
	for index, receipt := range frozen {
		number := uint32(index + 1)
		size, err := domain.ExpectedMultipartUploadPartSize(target.FileSizeBytes, target.PartSizeBytes, target.PartCount, number)
		if err != nil || receipt.PartNumber != number || receipt.SizeBytes != size ||
			!domain.ValidUploadPartETag(receipt.ETag) || !domain.ValidUploadPartChecksumSHA256(receipt.ChecksumSHA256) {
			return false
		}
	}
	return true
}

// compareUploadMergeReceipts 在冻结快照已校验的前提下比较完整存储清单。
func compareUploadMergeReceipts(target UploadPartSigningTarget, frozen, observed []domain.UploadPartReceipt) error {
	// 1. 复用分片范围和片长检查。无效存储结果不能作为失配证据。
	available, err := FilterUploadPartReceipts(target.FileSizeBytes, target.PartSizeBytes, target.PartCount, observed)
	if err != nil {
		return err
	}
	for _, receipt := range observed {
		if !domain.ValidUploadPartETag(receipt.ETag) || !domain.ValidUploadPartChecksumSHA256(receipt.ChecksumSHA256) {
			return domain.ErrUploadStorageUnavailable
		}
	}

	// 2. 按原计划顺序比较全部回执，最多报告 20 个失配编号。
	var mismatches []uint32
	receiptIndex := 0
	for _, expected := range frozen {
		if receiptIndex < len(available) && available[receiptIndex].PartNumber == expected.PartNumber {
			receipt := available[receiptIndex]
			receiptIndex++
			if receipt == expected {
				continue
			}
		}
		mismatches = append(mismatches, expected.PartNumber)
		if len(mismatches) == domain.MaxUploadPartsMismatchNumbers {
			break
		}
	}
	if len(mismatches) > 0 {
		return &domain.UploadPartsMismatchError{PartNumbers: mismatches}
	}
	return nil
}
