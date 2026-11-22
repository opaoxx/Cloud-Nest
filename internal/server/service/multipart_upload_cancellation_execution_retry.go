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
	nextAttemptAt, err := registerMultipartCleanupRetry(ctx, claimed, cause, reason, execution.scheduleRetry, "登记分片取消重试")
	if err != nil {
		return err
	}
	return &MultipartUploadCancellationRetryScheduledError{Cause: cause, NextAttemptAt: nextAttemptAt}
}

func registerMultipartCleanupRetry(
	ctx context.Context, claimed *domain.UploadTask, cause error, reason domain.MultipartUploadCleanupRetryReason,
	scheduleRetry func(context.Context, *domain.UploadTask, int, domain.MultipartUploadCleanupRetryReason) (*domain.UploadTask, error),
	operation string,
) (time.Time, error) {
	// 1. 确认调用仍有效，保留本轮失败原因。
	if ctx.Err() != nil || errors.Is(cause, context.Canceled) {
		return time.Time{}, errors.Join(cause, ctx.Err())
	}

	// 2. 按尝试次数计算退避，用当前收尾租约登记。
	delaySeconds := uploadRetryDelaySeconds(claimed.AttemptCount)
	scheduled, err := scheduleRetry(ctx, claimed, delaySeconds, reason)
	if err != nil {
		return time.Time{}, errors.Join(cause, fmt.Errorf("%s失败: %w", operation, err))
	}

	// 3. 核对原会话、预占和已提交的重试时间。
	if scheduled == nil || !sameMultipartCleanupTaskBinding(scheduled, claimed) ||
		scheduled.Status != claimed.Status || scheduled.ReservedBytes != claimed.ReservedBytes ||
		scheduled.LeaseOwner != nil || scheduled.LeaseUntil != nil || scheduled.CompletedAt != nil ||
		scheduled.PublishedObjectID != nil || scheduled.ResultEntryID != nil || scheduled.ResolvedName != nil ||
		scheduled.AttemptCount == 0 || scheduled.AttemptCount != claimed.AttemptCount+1 ||
		scheduled.UpdatedAt.Before(claimed.UpdatedAt) || !scheduled.UpdatedAt.Before(*claimed.LeaseUntil) ||
		(claimed.Status == domain.UploadStatusExpiring && scheduled.UpdatedAt.Before(claimed.ExpiresAt)) ||
		scheduled.LastErrorCode == nil || *scheduled.LastErrorCode != string(reason) || scheduled.NextAttemptAt == nil {
		return time.Time{}, errors.Join(cause, domain.ErrInvalidUploadTaskData)
	}
	expected := scheduled.UpdatedAt.Add(time.Duration(delaySeconds) * time.Second)
	if expected.Before(*claimed.LeaseUntil) {
		expected = *claimed.LeaseUntil
	}
	if !scheduled.NextAttemptAt.Equal(expected) {
		return time.Time{}, errors.Join(cause, domain.ErrInvalidUploadTaskData)
	}
	return *scheduled.NextAttemptAt, nil
}
