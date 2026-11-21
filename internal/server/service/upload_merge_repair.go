package service

import (
	"context"
	"errors"
	"slices"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CheckRepair 在明确失配后对原对象和会话做只读核对。
func (preflight *UploadMergePreflightService) CheckRepair(ctx context.Context, claimed *domain.UploadTask, failure error) (*domain.UploadMergeRepairEvidence, error) {
	// 1. 确认失配原因，固定当前租约绑定的目标。
	if !errors.Is(failure, domain.ErrUploadPartsMismatch) && !errors.Is(failure, domain.ErrMultipartUploadPartRejected) {
		return nil, domain.ErrUploadTaskNotReady
	}
	target, err := loadUploadMergeTarget(ctx, claimed, preflight.recovery.getTarget)
	if err != nil {
		return nil, err
	}
	if err := preflight.checkRepairObjectMissing(ctx, claimed, target); err != nil {
		return nil, err
	}

	// 2. 核对原会话仍可读取，并重新比对冻结回执。
	frozen, observed, err := preflight.receipts.readTargetReceipts(ctx, claimed, target)
	if err != nil {
		return nil, err
	}
	var mismatch *domain.UploadPartsMismatchError
	err = compareUploadMergeReceipts(target, frozen, observed)
	if err == nil {
		return nil, domain.ErrUploadTaskNotReady
	}
	if !errors.As(err, &mismatch) {
		return nil, err
	}

	// 3. 再确认原对象缺失，返回仍失配的分片编号。
	if err := preflight.checkRepairObjectMissing(ctx, claimed, target); err != nil {
		return nil, err
	}
	return &domain.UploadMergeRepairEvidence{Task: *copyUploadTask(claimed), ObjectKey: target.ObjectKey, PartNumbers: slices.Clone(mismatch.PartNumbers)}, nil
}

func (preflight *UploadMergePreflightService) checkRepairObjectMissing(ctx context.Context, claimed *domain.UploadTask, target UploadPartSigningTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := preflight.recovery.checkTarget(ctx, claimed, target)
	if err == nil {
		return domain.ErrUploadObjectExists
	}
	if errors.Is(err, domain.ErrUploadObjectMissing) {
		return nil
	}
	return err
}
