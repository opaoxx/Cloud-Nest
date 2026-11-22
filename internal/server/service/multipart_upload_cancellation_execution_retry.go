package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadCancellationRetryScheduledError 表示下次取消收尾已经登记。
type MultipartUploadCancellationRetryScheduledError struct {
	Cause         error
	NextAttemptAt time.Time
}

func (scheduled *MultipartUploadCancellationRetryScheduledError) Error() string {
	return "已登记分片取消重试"
}

func (scheduled *MultipartUploadCancellationRetryScheduledError) Unwrap() error {
	return scheduled.Cause
}

func (execution *MultipartUploadCancellationExecutionService) retryAfterStorageObservation(
	ctx context.Context, claimed *domain.UploadTask, cause error,
) error {
	if !errors.Is(cause, domain.ErrUploadStorageUnavailable) {
		return cause
	}
	return execution.registerCancellationRetry(ctx, claimed, cause, domain.MultipartCancellationRetryStorage)
}

func (execution *MultipartUploadCancellationExecutionService) retryAfterCancellationWrite(
	ctx context.Context, claimed *domain.UploadTask, cause error,
) error {
	if !errors.Is(cause, domain.ErrUploadCancellationRetryable) {
		return cause
	}
	return execution.registerCancellationRetry(ctx, claimed, cause, domain.MultipartCancellationRetryDatabase)
}

func (execution *MultipartUploadCancellationExecutionService) registerCancellationRetry(
	ctx context.Context, claimed *domain.UploadTask, cause error, reason domain.MultipartUploadCancellationRetryReason,
) error {
	// 1. 确认调用仍有效，保留本轮取消原因。
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) {
		return errors.Join(cause, ctx.Err())
	}

	// 2. 按尝试次数计算退避，用当前租约登记。
	delaySeconds := uploadRetryDelaySeconds(claimed.AttemptCount)
	scheduled, err := execution.scheduleRetry(ctx, claimed, delaySeconds, reason)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("登记分片取消重试失败: %w", err))
	}

	// 3. 核对已提交的重试时间和任务绑定，再返回登记信号。
	if scheduled == nil || !sameMultipartCancellationTaskBinding(scheduled, claimed) ||
		scheduled.Status != domain.UploadStatusCancelling || scheduled.ReservedBytes != claimed.ReservedBytes ||
		scheduled.LeaseOwner != nil || scheduled.LeaseUntil != nil || scheduled.CompletedAt != nil ||
		scheduled.PublishedObjectID != nil || scheduled.ResultEntryID != nil || scheduled.ResolvedName != nil ||
		scheduled.AttemptCount == 0 || scheduled.AttemptCount != claimed.AttemptCount+1 ||
		scheduled.UpdatedAt.Before(claimed.UpdatedAt) || !scheduled.UpdatedAt.Before(*claimed.LeaseUntil) ||
		scheduled.LastErrorCode == nil || *scheduled.LastErrorCode != string(reason) || scheduled.NextAttemptAt == nil {
		return errors.Join(cause, domain.ErrInvalidUploadTaskData)
	}
	expected := scheduled.UpdatedAt.Add(time.Duration(delaySeconds) * time.Second)
	if expected.Before(*claimed.LeaseUntil) {
		expected = *claimed.LeaseUntil
	}
	if !scheduled.NextAttemptAt.Equal(expected) {
		return errors.Join(cause, domain.ErrInvalidUploadTaskData)
	}
	return &MultipartUploadCancellationRetryScheduledError{Cause: cause, NextAttemptAt: *scheduled.NextAttemptAt}
}
