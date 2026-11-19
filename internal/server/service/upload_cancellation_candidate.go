package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadCancellationCandidateProcessor 串联候选读取、取消收尾和重试安排。
type UploadCancellationCandidateProcessor struct {
	findTask      func(context.Context, uint64, uint64) (*domain.UploadTask, error)
	complete      func(context.Context, *domain.UploadTask) (*domain.UploadTask, error)
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error)
}

// NewUploadCancellationCandidateProcessor 绑定归属读取、收尾和重试写回事务。
func NewUploadCancellationCandidateProcessor(
	findTask func(context.Context, uint64, uint64) (*domain.UploadTask, error),
	complete func(context.Context, *domain.UploadTask) (*domain.UploadTask, error),
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error),
) *UploadCancellationCandidateProcessor {
	// 1. 核对读取和写入依赖已装配。
	if findTask == nil || complete == nil || scheduleRetry == nil {
		panic("处理取消上传候选需要任务读取、取消收尾和重试写回方法")
	}
	return &UploadCancellationCandidateProcessor{findTask: findTask, complete: complete, scheduleRetry: scheduleRetry}
}

// Process 处理一条已扫描的单文件取消候选。
// 返回已确认取消或已安排重试的任务。
func (processor *UploadCancellationCandidateProcessor) Process(ctx context.Context, candidate domain.UploadCancellationCandidate) (*domain.UploadTask, error) {
	// 1. 检查候选和调用状态，按所属用户重新读取任务。
	if ctx == nil {
		return nil, errors.New("处理取消上传候选需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if candidate.TaskID == 0 || candidate.UserID == 0 || candidate.StagedObjectID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	task, err := processor.findTask(ctx, candidate.UserID, candidate.TaskID)
	if err != nil {
		return nil, err
	}

	// 2. 核对身份、阶段、暂存对象和租约版本。
	if task == nil || task.ID != candidate.TaskID || task.UserID != candidate.UserID {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.UploadMode != "SINGLE" || (task.Status != domain.UploadStatusCancelling && task.Status != domain.UploadStatusCancelled) {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.StagedObjectID != candidate.StagedObjectID || task.LeaseVersion != candidate.LeaseVersion {
		return nil, domain.ErrUploadTaskBusy
	}

	// 3. 调用取消收尾事务，由数据库再次检查写入条件。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finished, completeError := processor.complete(ctx, task)

	// 4. 确认可重试的收尾错误，按尝试次数安排下次处理。
	if !errors.Is(completeError, domain.ErrUploadCancellationRetryable) || ctx.Err() != nil || task.Status != domain.UploadStatusCancelling {
		return finished, completeError
	}
	delaySeconds := uploadRetryDelaySeconds(task.AttemptCount)
	return processor.scheduleRetry(ctx, task, delaySeconds)
}
