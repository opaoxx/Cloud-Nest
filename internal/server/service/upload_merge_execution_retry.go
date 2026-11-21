package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadMergeRetryScheduledError 表示合并尚未完成，但下次恢复核对已登记。
type UploadMergeRetryScheduledError struct {
	Cause         error
	NextAttemptAt time.Time
}

func (scheduled *UploadMergeRetryScheduledError) Error() string { return "已登记分片合并重试" }

func (scheduled *UploadMergeRetryScheduledError) Unwrap() error { return scheduled.Cause }

func (execution *UploadMergeExecutionService) scheduleStorageRetry(ctx context.Context, claimed *domain.UploadTask, cause error) error {
	retryable := errors.Is(cause, domain.ErrUploadStorageUnavailable) || errors.Is(cause, context.DeadlineExceeded)
	return execution.registerRetry(ctx, claimed, cause, retryable)
}

func (execution *UploadMergeExecutionService) registerRetry(ctx context.Context, claimed *domain.UploadTask, cause error, retryable bool) error {
	// 1. 判断失败是否允许登记重试，保留原错误。
	if !retryable || ctx.Err() != nil || errors.Is(cause, context.Canceled) {
		return cause
	}

	// 2. 按已有退避规则，用当前租约写入下次核对时间。
	scheduled, err := execution.scheduleRetry(ctx, claimed, uploadRetryDelaySeconds(claimed.AttemptCount))

	// 3. 返回登记结果，或同时保留执行与登记失败的原因。
	if err != nil {
		return errors.Join(cause, fmt.Errorf("登记分片合并重试失败: %w", err))
	}
	if scheduled == nil || scheduled.Status != domain.UploadStatusCompleting || scheduled.UploadMode != claimed.UploadMode ||
		scheduled.ID != claimed.ID || scheduled.UserID != claimed.UserID || scheduled.StagedObjectID != claimed.StagedObjectID ||
		scheduled.StorageUploadID == nil || *scheduled.StorageUploadID != *claimed.StorageUploadID ||
		scheduled.LeaseVersion != claimed.LeaseVersion || scheduled.LeaseOwner != nil || scheduled.LeaseUntil != nil ||
		scheduled.AttemptCount == 0 || scheduled.AttemptCount != claimed.AttemptCount+1 || scheduled.NextAttemptAt == nil ||
		!scheduled.NextAttemptAt.After(scheduled.UpdatedAt) || scheduled.NextAttemptAt.After(claimed.ExpiresAt) {
		return errors.Join(cause, domain.ErrInvalidUploadTaskData)
	}
	return &UploadMergeRetryScheduledError{Cause: cause, NextAttemptAt: *scheduled.NextAttemptAt}
}
