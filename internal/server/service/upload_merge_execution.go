package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadMergeExecutionService 执行已领取的单条 Multipart 合并任务。
type UploadMergeExecutionService struct {
	provider, bucket  string
	preflight         func(context.Context, *domain.UploadTask) (*UploadMergePreflightResult, error)
	complete          func(context.Context, string, string, uint64, uint64, uint32, []domain.UploadPartReceipt) (*domain.UploadObjectResult, error)
	beginVerification func(context.Context, *domain.UploadTask, string, string, string, domain.UploadObjectResult) (*domain.UploadTask, error)
	scheduleRetry     func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error)
	checkRepair       func(context.Context, *domain.UploadTask, error) (*domain.UploadMergeRepairEvidence, error)
	reopenForRepair   func(context.Context, string, string, *domain.UploadMergeRepairEvidence) (*domain.UploadTask, error)
}

// NewUploadMergeExecutionService 绑定合并、校验写回、重试登记及补传所需的操作。
func NewUploadMergeExecutionService(
	provider, bucket string,
	preflight func(context.Context, *domain.UploadTask) (*UploadMergePreflightResult, error),
	complete func(context.Context, string, string, uint64, uint64, uint32, []domain.UploadPartReceipt) (*domain.UploadObjectResult, error),
	beginVerification func(context.Context, *domain.UploadTask, string, string, string, domain.UploadObjectResult) (*domain.UploadTask, error),
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error),
	checkRepair func(context.Context, *domain.UploadTask, error) (*domain.UploadMergeRepairEvidence, error),
	reopenForRepair func(context.Context, string, string, *domain.UploadMergeRepairEvidence) (*domain.UploadTask, error),
) *UploadMergeExecutionService {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || preflight == nil || complete == nil || beginVerification == nil || scheduleRetry == nil ||
		checkRepair == nil || reopenForRepair == nil {
		panic("单条分片合并执行需要存储配置、预检、合并、校验写回、重试登记和补传方法")
	}
	return &UploadMergeExecutionService{provider: provider, bucket: bucket, preflight: preflight, complete: complete,
		beginVerification: beginVerification, scheduleRetry: scheduleRetry, checkRepair: checkRepair, reopenForRepair: reopenForRepair}
}

// Execute 只用当前租约执行一次。成功返回已提交的 VERIFYING 或 UPLOADING 任务。
// 可重试错误登记后返回 UploadMergeRetryScheduledError，保留原原因。
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
		return execution.handleStorageFailure(ctx, claimed, "", err)
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
			return execution.handleStorageFailure(ctx, claimed, target.ObjectKey, err)
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
	if committed == nil || committed.Status != domain.UploadStatusVerifying || !sameMergeExecutionBinding(committed, claimed) ||
		committed.LeaseOwner != nil || committed.LeaseUntil != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return committed, nil
}

func (execution *UploadMergeExecutionService) handleStorageFailure(ctx context.Context, claimed *domain.UploadTask, attemptedKey string, cause error) (*domain.UploadTask, error) {
	// 1. 区分明确失配与沿用原规则处理的存储错误。
	if !errors.Is(cause, domain.ErrUploadPartsMismatch) && !errors.Is(cause, domain.ErrMultipartUploadPartRejected) {
		return nil, execution.scheduleStorageRetry(ctx, claimed, cause)
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(cause, err)
	}

	// 2. 用当前租约复查原对象和原会话，取得本轮修复证据。
	evidence, err := execution.checkRepair(ctx, claimed, cause)
	if err != nil {
		return nil, errors.Join(cause, fmt.Errorf("核对分片补传条件失败: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(cause, err)
	}
	if evidence == nil || evidence.ObjectKey == "" || !validUploadMergeClaim(&evidence.Task) || !sameMergeExecutionBinding(&evidence.Task, claimed) ||
		*evidence.Task.LeaseOwner != *claimed.LeaseOwner || !evidence.Task.LeaseUntil.Equal(*claimed.LeaseUntil) ||
		(attemptedKey != "" && evidence.ObjectKey != attemptedKey) {
		return nil, errors.Join(cause, domain.ErrInvalidUploadTaskData)
	}

	// 3. 提交补传事务并返回任务，保留复查或写回失败的原因。
	reopened, err := execution.reopenForRepair(ctx, execution.provider, execution.bucket, evidence)
	if err != nil {
		return nil, errors.Join(cause, fmt.Errorf("恢复分片补传失败: %w", err))
	}
	if reopened == nil || reopened.Status != domain.UploadStatusUploading || !sameMergeExecutionBinding(reopened, claimed) ||
		reopened.LeaseOwner != nil || reopened.LeaseUntil != nil {
		return nil, errors.Join(cause, domain.ErrInvalidUploadTaskData)
	}
	return reopened, nil
}

func sameMergeExecutionBinding(task, claimed *domain.UploadTask) bool {
	return task.ID == claimed.ID && task.UserID == claimed.UserID && task.StagedObjectID == claimed.StagedObjectID &&
		task.UploadMode == claimed.UploadMode && task.LeaseVersion == claimed.LeaseVersion &&
		task.StorageUploadID != nil && *task.StorageUploadID == *claimed.StorageUploadID &&
		task.FileSize == claimed.FileSize && task.ExpiresAt.Equal(claimed.ExpiresAt) &&
		task.PartSize != nil && *task.PartSize == *claimed.PartSize && task.PartCount != nil && *task.PartCount == *claimed.PartCount
}
