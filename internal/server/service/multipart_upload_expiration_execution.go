package service

import (
	"context"
	"errors"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadExpirationExecutionService 确认一条已领取租约的 Multipart 到期任务。
type MultipartUploadExpirationExecutionService struct {
	cleanup       multipartUploadCleanupExecution
	scheduleRetry func(context.Context, *domain.UploadTask, int, domain.MultipartUploadCleanupRetryReason) (*domain.UploadTask, error)
}

// NewMultipartUploadExpirationExecutionService 绑定存储观察、终态事务和重试事务。
func NewMultipartUploadExpirationExecutionService(
	provider, bucket string,
	observeBound, observeUnbound func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCleanupObservation, error),
	completeMissing, completeExisting func(context.Context, string, string, *domain.MultipartUploadCleanupObservation) (*domain.UploadTask, error),
	scheduleRetry func(context.Context, *domain.UploadTask, int, domain.MultipartUploadCleanupRetryReason) (*domain.UploadTask, error),
) *MultipartUploadExpirationExecutionService {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || observeBound == nil || observeUnbound == nil || completeMissing == nil || completeExisting == nil || scheduleRetry == nil {
		panic("单条分片到期执行需要存储配置、观察、终态和重试事务")
	}
	return &MultipartUploadExpirationExecutionService{
		cleanup: multipartUploadCleanupExecution{provider: provider, bucket: bucket, observeBound: observeBound, observeUnbound: observeUnbound,
			completeMissing: completeMissing, completeExisting: completeExisting},
		scheduleRetry: scheduleRetry,
	}
}

// Execute 按当前租约处理一次，返回 EXPIRED 或已提交的重试登记信号。
func (execution *MultipartUploadExpirationExecutionService) Execute(ctx context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 区分待确认、可重试的存储故障和应直接返回的错误。
	observation, err := execution.cleanup.observe(ctx, claimed, domain.UploadStatusExpiring)
	if errors.Is(err, domain.ErrUploadStorageUnavailable) {
		return nil, execution.registerExpirationRetry(ctx, claimed, err, domain.MultipartCleanupRetryStorage)
	}
	if err != nil {
		return nil, err
	}
	if observation == nil {
		return nil, execution.registerExpirationRetry(ctx, claimed, domain.ErrUploadStorageUnavailable, domain.MultipartCleanupRetryStorage)
	}
	// 2. 提交已确认的观察。已确认的临时数据库错误进入重试登记。
	finished, err := execution.cleanup.complete(ctx, claimed, observation, domain.UploadStatusExpired)
	if errors.Is(err, domain.ErrUploadExpirationRetryable) {
		return nil, execution.registerExpirationRetry(ctx, claimed, err, domain.MultipartCleanupRetryDatabase)
	}
	return finished, err
}

// MultipartUploadExpirationRetryScheduledError 表示下次到期收尾已经登记。
type MultipartUploadExpirationRetryScheduledError struct {
	Cause         error
	NextAttemptAt time.Time
}

func (scheduled *MultipartUploadExpirationRetryScheduledError) Error() string {
	return "已登记分片到期重试"
}

func (scheduled *MultipartUploadExpirationRetryScheduledError) Unwrap() error {
	return scheduled.Cause
}

func (execution *MultipartUploadExpirationExecutionService) registerExpirationRetry(
	ctx context.Context, claimed *domain.UploadTask, cause error, reason domain.MultipartUploadCleanupRetryReason,
) error {
	nextAttemptAt, err := registerMultipartCleanupRetry(ctx, claimed, cause, reason, execution.scheduleRetry, "登记分片到期重试")
	if err != nil {
		return err
	}
	return &MultipartUploadExpirationRetryScheduledError{Cause: cause, NextAttemptAt: nextAttemptAt}
}
