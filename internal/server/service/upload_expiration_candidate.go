package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadExpirationCandidateProcessor 串联一次单文件过期推进、收尾和重试安排。
type UploadExpirationCandidateProcessor struct {
	begin         func(context.Context, domain.UploadExpirationCandidate) (*domain.UploadTask, error)
	complete      func(context.Context, *domain.UploadTask) (*domain.UploadTask, error)
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error)
}

// NewUploadExpirationCandidateProcessor 绑定推进、收尾和重试写回事务。
func NewUploadExpirationCandidateProcessor(
	begin func(context.Context, domain.UploadExpirationCandidate) (*domain.UploadTask, error),
	complete func(context.Context, *domain.UploadTask) (*domain.UploadTask, error),
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error),
) *UploadExpirationCandidateProcessor {
	// 1. 校验构造依赖，保留三个事务入口。
	if begin == nil || complete == nil || scheduleRetry == nil {
		panic("处理到期上传候选需要阶段推进、收尾和重试写回方法")
	}
	return &UploadExpirationCandidateProcessor{begin: begin, complete: complete, scheduleRetry: scheduleRetry}
}

// Process 根据单个到期候选返回已终结或已安排重试的单文件任务。
func (processor *UploadExpirationCandidateProcessor) Process(ctx context.Context, candidate domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
	// 1. 核对候选是否属于单文件上传。
	if candidate.UploadMode != "SINGLE" {
		return nil, domain.ErrUploadTaskNotReady
	}

	// 2. 复核并推进到 EXPIRING，取得已提交的任务。
	expiring, err := processor.begin(ctx, candidate)
	if err != nil {
		return nil, err
	}
	if expiring == nil || expiring.Status != domain.UploadStatusExpiring || expiring.UploadMode != candidate.UploadMode ||
		expiring.ID != candidate.TaskID || expiring.UserID != candidate.UserID || expiring.StagedObjectID != candidate.StagedObjectID ||
		expiring.LeaseVersion != candidate.LeaseVersion || !expiring.ExpiresAt.Equal(candidate.ExpiresAt) {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 执行一次收尾，检查失败原因和当前调用状态。
	finished, completeError := processor.complete(ctx, expiring)
	if !errors.Is(completeError, domain.ErrUploadExpirationRetryable) || ctx.Err() != nil {
		return finished, completeError
	}

	// 4. 按尝试次数计算等待时间，提交重试安排。
	delaySeconds := uploadRetryDelaySeconds(expiring.AttemptCount)
	return processor.scheduleRetry(ctx, expiring, delaySeconds)
}
