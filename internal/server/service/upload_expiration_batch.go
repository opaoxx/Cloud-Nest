package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadExpirationCandidateError 记录单条到期候选的处理错误。
type UploadExpirationCandidateError struct {
	TaskID uint64
	Err    error
}

// UploadExpirationBatchResult 汇总一轮到期扫描及已经处理的结果。
type UploadExpirationBatchResult struct {
	Scanned          int
	Expired          int
	RetriesScheduled int
	CandidateErrors  []UploadExpirationCandidateError
}

// UploadExpirationBatch 将单文件扫描接到逐条过期处理入口。
type UploadExpirationBatch struct {
	list    func(context.Context, int) ([]domain.UploadExpirationCandidate, error)
	process func(context.Context, domain.UploadExpirationCandidate) (*domain.UploadTask, error)
}

// NewUploadExpirationBatch 绑定单文件扫描和候选处理方法。
func NewUploadExpirationBatch(
	list func(context.Context, int) ([]domain.UploadExpirationCandidate, error),
	process func(context.Context, domain.UploadExpirationCandidate) (*domain.UploadTask, error),
) *UploadExpirationBatch {
	// 1. 检查扫描和处理依赖。
	if list == nil || process == nil {
		panic("批量上传过期处理需要单文件扫描和候选处理方法")
	}
	return &UploadExpirationBatch{list: list, process: process}
}

// RunOnce 扫描一次，逐条处理到期单文件任务。
// 扫描和取消错误直接返回，单条处理错误保留在汇总中。
func (batch *UploadExpirationBatch) RunOnce(ctx context.Context, limit int) (UploadExpirationBatchResult, error) {
	// 1. 按批量上限扫描一次 SINGLE 到期候选。
	var summary UploadExpirationBatchResult
	if ctx == nil {
		return summary, fmt.Errorf("批量处理到期上传需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	candidates, err := batch.list(ctx, limit)
	if err != nil {
		return summary, err
	}
	if len(candidates) > limit {
		return summary, domain.ErrInvalidUploadTaskData
	}
	summary.Scanned = len(candidates)

	// 2. 按顺序处理候选，统计已终结、已安排重试和逐条错误。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		task, err := batch.process(ctx, candidate)
		if err == nil {
			switch {
			case task == nil || task.ID != candidate.TaskID || task.UserID != candidate.UserID ||
				task.StagedObjectID != candidate.StagedObjectID || task.UploadMode != "SINGLE":
				err = domain.ErrInvalidUploadTaskData
			case task.Status == domain.UploadStatusExpired && task.ReservedBytes == 0:
				summary.Expired++
			case task.Status == domain.UploadStatusExpiring && task.CompletedAt == nil &&
				task.NextAttemptAt != nil && task.NextAttemptAt.After(task.UpdatedAt):
				summary.RetriesScheduled++
			default:
				err = domain.ErrInvalidUploadTaskData
			}
		}
		if err != nil {
			summary.CandidateErrors = append(summary.CandidateErrors, UploadExpirationCandidateError{TaskID: candidate.TaskID, Err: err})
		}
	}

	// 3. 返回本轮汇总及可能的取消原因。
	return summary, ctx.Err()
}
