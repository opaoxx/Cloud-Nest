package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadCancellationExecutionService 处理一条已领取租约的 Multipart 取消任务。
type MultipartUploadCancellationExecutionService struct {
	cleanup       multipartUploadCleanupExecution
	scheduleRetry func(context.Context, *domain.UploadTask, int, domain.MultipartUploadCancellationRetryReason) (*domain.UploadTask, error)
}

// NewMultipartUploadCancellationExecutionService 绑定同一存储配置的观察及终态事务。
func NewMultipartUploadCancellationExecutionService(
	provider, bucket string,
	observeBound, observeUnbound func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCancellationObservation, error),
	completeMissing, completeExisting func(context.Context, string, string, *domain.MultipartUploadCancellationObservation) (*domain.UploadTask, error),
	scheduleRetry func(context.Context, *domain.UploadTask, int, domain.MultipartUploadCancellationRetryReason) (*domain.UploadTask, error),
) *MultipartUploadCancellationExecutionService {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || observeBound == nil || observeUnbound == nil || completeMissing == nil || completeExisting == nil || scheduleRetry == nil {
		panic("单条分片取消执行需要存储配置、观察、终态和重试事务")
	}
	return &MultipartUploadCancellationExecutionService{
		cleanup: multipartUploadCleanupExecution{provider: provider, bucket: bucket, observeBound: observeBound, observeUnbound: observeUnbound,
			completeMissing: completeMissing, completeExisting: completeExisting},
		scheduleRetry: scheduleRetry,
	}
}

// Execute 按当前租约处理一次，返回 CANCELLED 或已提交的重试登记信号。
func (execution *MultipartUploadCancellationExecutionService) Execute(ctx context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 按原会话绑定选择观察方式。存储暂时不能确认时登记重试。
	observation, err := execution.cleanup.observe(ctx, claimed, domain.UploadStatusCancelling)
	if err != nil {
		return nil, execution.retryAfterStorageObservation(ctx, claimed, err)
	}
	if observation == nil {
		return nil, execution.registerCancellationRetry(ctx, claimed, domain.ErrUploadStorageUnavailable, domain.MultipartCancellationRetryStorage)
	}

	// 2. 交付已确认的观察并核对终态。临时数据库错误可安排重试。
	finished, err := execution.cleanup.complete(ctx, claimed, observation, domain.UploadStatusCancelled)
	if err != nil {
		return nil, execution.retryAfterCancellationWrite(ctx, claimed, err)
	}

	return finished, nil
}

func validMultipartCancellationCompletion(finished, claimed *domain.UploadTask) bool {
	return validMultipartCleanupCompletion(finished, claimed, domain.UploadStatusCancelled)
}
