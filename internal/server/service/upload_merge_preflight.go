package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadMergePreflightResult 保存本轮只读检查的成功依据，两个字段中恰有一个非空。
type UploadMergePreflightResult struct {
	Recovered *UploadMergeRecoveryObservation
	Ready     *UploadMergeReceiptSnapshot
}

// UploadMergePreflightService 按当前合并租约先查原对象，再按需核对冻结回执。
type UploadMergePreflightService struct {
	recovery *UploadMergeRecoveryService
	receipts *UploadMergeReceiptService
}

// NewUploadMergePreflightService 组合现有对象恢复和冻结回执检查，两者应使用同一存储配置。
func NewUploadMergePreflightService(recovery *UploadMergeRecoveryService, receipts *UploadMergeReceiptService) *UploadMergePreflightService {
	if recovery == nil || receipts == nil {
		panic("分片合并前检查需要对象恢复和冻结回执服务")
	}
	return &UploadMergePreflightService{recovery: recovery, receipts: receipts}
}

// Check 只读检查本轮可使用的依据。未知状态和失配返回对应错误，供后续流程判断。
func (preflight *UploadMergePreflightService) Check(ctx context.Context, claimed *domain.UploadTask) (*UploadMergePreflightResult, error) {
	// 1. 固定当前合并租约绑定的存储目标。
	target, err := loadUploadMergeTarget(ctx, claimed, preflight.recovery.getTarget)
	if err != nil {
		return nil, err
	}

	// 2. 先检查原对象，交付可恢复的对象元数据。
	observation, err := preflight.recovery.checkTarget(ctx, claimed, target)
	if err == nil {
		return &UploadMergePreflightResult{Recovered: observation}, nil
	}
	if !errors.Is(err, domain.ErrUploadObjectMissing) {
		return nil, err
	}

	// 3. 仅在对象明确缺失时核对同一目标的分片回执。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	receipts, err := preflight.receipts.verifyTarget(ctx, claimed, target)
	if err != nil {
		return nil, err
	}
	return &UploadMergePreflightResult{Ready: receipts}, nil
}
