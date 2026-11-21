package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadMergeExecutionService 将已领取的单条 Multipart 任务推进至校验。
type UploadMergeExecutionService struct {
	provider, bucket  string
	preflight         func(context.Context, *domain.UploadTask) (*UploadMergePreflightResult, error)
	complete          func(context.Context, string, string, uint64, uint64, uint32, []domain.UploadPartReceipt) (*domain.UploadObjectResult, error)
	beginVerification func(context.Context, *domain.UploadTask, string, string, string, domain.UploadObjectResult) (*domain.UploadTask, error)
	scheduleRetry     func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error)
}

// NewUploadMergeExecutionService 绑定预检、合并适配、校验状态写回和重试登记。
func NewUploadMergeExecutionService(
	provider, bucket string,
	preflight func(context.Context, *domain.UploadTask) (*UploadMergePreflightResult, error),
	complete func(context.Context, string, string, uint64, uint64, uint32, []domain.UploadPartReceipt) (*domain.UploadObjectResult, error),
	beginVerification func(context.Context, *domain.UploadTask, string, string, string, domain.UploadObjectResult) (*domain.UploadTask, error),
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error),
) *UploadMergeExecutionService {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || preflight == nil || complete == nil || beginVerification == nil || scheduleRetry == nil {
		panic("单条分片合并执行需要存储配置、预检、合并、校验写回及重试登记方法")
	}
	return &UploadMergeExecutionService{provider: provider, bucket: bucket, preflight: preflight, complete: complete,
		beginVerification: beginVerification, scheduleRetry: scheduleRetry}
}

// Execute 只用当前租约执行一次。可重试错误登记后返回 UploadMergeRetryScheduledError，保留原原因。
func (execution *UploadMergeExecutionService) Execute(ctx context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 复核任务和预检结果，固定本次执行的目标。
	if ctx == nil || !validUploadMergeClaim(claimed) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	preflight, err := execution.preflight(ctx, claimed)
	if err != nil {
		return nil, execution.scheduleStorageRetry(ctx, claimed, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if preflight == nil || (preflight.Recovered == nil) == (preflight.Ready == nil) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 2. 使用已恢复的对象，或向原会话提交一次合并。
	var target UploadPartSigningTarget
	var confirmation *domain.UploadObjectResult
	if recovered := preflight.Recovered; recovered != nil {
		target, confirmation = recovered.Target, &recovered.Object
		if !matchesClaimedUploadPartTarget(&target, claimed) {
			return nil, domain.ErrInvalidUploadTaskData
		}
	} else {
		ready := preflight.Ready
		target = ready.Target
		if !matchesClaimedUploadPartTarget(&target, claimed) || !validFrozenMergeReceipts(target, ready.Parts) {
			return nil, domain.ErrInvalidUploadTaskData
		}
		confirmation, err = execution.complete(ctx, target.ObjectKey, target.UploadID, target.FileSizeBytes, target.PartSizeBytes, target.PartCount, ready.Parts)
		if err != nil {
			return nil, execution.scheduleStorageRetry(ctx, claimed, err)
		}
	}

	// 3. 保存确认的对象元数据，返回已提交的 VERIFYING 任务。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateUploadMergeObjectResult(confirmation, target.FileSizeBytes); err != nil {
		return nil, err
	}
	committed, err := execution.beginVerification(ctx, claimed, execution.provider, execution.bucket, target.ObjectKey, *confirmation)
	if err != nil {
		return nil, execution.registerRetry(ctx, claimed, err, errors.Is(err, domain.ErrUploadMergeWriteRetryable))
	}
	if committed == nil || committed.Status != domain.UploadStatusVerifying || committed.UploadMode != claimed.UploadMode ||
		committed.StorageUploadID == nil || committed.PartSize == nil || committed.PartCount == nil ||
		!matchesClaimedUploadPartTarget(&target, committed) || committed.LeaseOwner != nil || committed.LeaseUntil != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return committed, nil
}
