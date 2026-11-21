package service

import (
	"context"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadMergeRecoveryObservation 保存与当前租约匹配的目标和已观察到的对象元数据。
type UploadMergeRecoveryObservation struct {
	Target UploadPartSigningTarget
	Object domain.UploadObjectResult
}

// UploadMergeRecoveryService 在合并结果不明时只读检查原对象键。
type UploadMergeRecoveryService struct {
	getTarget  func(context.Context, *domain.UploadTask) (*UploadPartSigningTarget, error)
	statObject func(context.Context, string) (*domain.UploadObjectResult, error)
}

// NewUploadMergeRecoveryService 绑定持租约的目标查询和只读 HEAD 适配。
func NewUploadMergeRecoveryService(
	getTarget func(context.Context, *domain.UploadTask) (*UploadPartSigningTarget, error),
	statObject func(context.Context, string) (*domain.UploadObjectResult, error),
) *UploadMergeRecoveryService {
	if getTarget == nil || statObject == nil {
		panic("后台合并恢复检查需要目标查询和对象元数据读取方法")
	}
	return &UploadMergeRecoveryService{getTarget: getTarget, statObject: statObject}
}

// Check 只交付大小匹配的对象元数据，保留明确缺失和查询故障供后续流程判断。
// 调用方推进状态前仍须复查租约，文件内容由校验阶段检查。
func (recovery *UploadMergeRecoveryService) Check(ctx context.Context, claimed *domain.UploadTask) (*UploadMergeRecoveryObservation, error) {
	// 1. 复核合并租约，固定原对象键和声明大小。
	target, err := loadUploadMergeTarget(ctx, claimed, recovery.getTarget)
	if err != nil {
		return nil, err
	}

	// 2. 只读查询对象元数据。
	observed, statError := recovery.statObject(ctx, target.ObjectKey)

	// 3. 再次复查租约和目标，核对大小后交付结果。
	if err := recheckUploadMergeTarget(ctx, claimed, target, recovery.getTarget); err != nil {
		return nil, err
	}
	if statError != nil {
		return nil, statError
	}
	if observed == nil || observed.StorageETag == nil || strings.TrimSpace(*observed.StorageETag) == "" ||
		(observed.StorageVersionID != nil && *observed.StorageVersionID == "") {
		return nil, domain.ErrUploadStorageUnavailable
	}
	if observed.SizeBytes != target.FileSizeBytes {
		return nil, domain.ErrUploadBodySizeMismatch
	}
	return &UploadMergeRecoveryObservation{Target: target, Object: domain.UploadObjectResult{
		SizeBytes: observed.SizeBytes, StorageETag: copyOptionalValue(observed.StorageETag), StorageVersionID: copyOptionalValue(observed.StorageVersionID),
	}}, nil
}
