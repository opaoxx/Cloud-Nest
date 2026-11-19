package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadVerificationCandidateError 记录单条候选的处理错误。
type UploadVerificationCandidateError struct {
	TaskID uint64
	Err    error
}

// UploadVerificationBatchResult 汇总一轮扫描及已完成的处理结果。
type UploadVerificationBatchResult struct {
	Scanned          int
	Published        int
	RetriesScheduled int
	CandidateErrors  []UploadVerificationCandidateError
}

// UploadVerificationBatch 将一次扫描接到顺序执行的候选处理入口。
type UploadVerificationBatch struct {
	list    func(context.Context, int) ([]domain.UploadVerificationCandidate, error)
	process func(context.Context, domain.UploadVerificationCandidate, string) (*domain.UploadTask, error)
}

// NewUploadVerificationBatch 绑定扫描及单条处理方法。
func NewUploadVerificationBatch(
	list func(context.Context, int) ([]domain.UploadVerificationCandidate, error),
	process func(context.Context, domain.UploadVerificationCandidate, string) (*domain.UploadTask, error),
) *UploadVerificationBatch {
	// 1. 核对扫描和处理方法已装配，保存供每轮调用。
	if list == nil || process == nil {
		panic("批量上传校验需要扫描和单条处理方法")
	}
	return &UploadVerificationBatch{list: list, process: process}
}

// RunOnce 扫描一次并逐条处理。扫描或上下文错误交给调用方，单条错误保存在汇总中。
func (batch *UploadVerificationBatch) RunOnce(ctx context.Context, limit int, leaseOwner string) (UploadVerificationBatchResult, error) {
	// 1. 检查上下文，按批量上限扫描一次候选。
	var summary UploadVerificationBatchResult
	if ctx == nil {
		return summary, fmt.Errorf("扫描并处理上传校验候选需要上下文")
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

	// 2. 按顺序处理候选，记录发布、重试及逐条错误。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		task, err := batch.process(ctx, candidate, leaseOwner)
		if err == nil {
			switch {
			case task == nil || task.ID != candidate.TaskID || task.UserID != candidate.UserID || task.StagedObjectID != candidate.StagedObjectID:
				err = domain.ErrInvalidUploadTaskData
			case task.Status == domain.UploadStatusCompleted:
				summary.Published++
			case task.Status == domain.UploadStatusVerifying && task.NextAttemptAt != nil && task.LeaseOwner == nil && task.LeaseUntil == nil:
				summary.RetriesScheduled++
			default:
				err = domain.ErrInvalidUploadTaskData
			}
		}
		if err != nil {
			summary.CandidateErrors = append(summary.CandidateErrors, UploadVerificationCandidateError{TaskID: candidate.TaskID, Err: err})
		}
	}

	// 3. 返回本轮汇总及可能的取消原因。
	return summary, ctx.Err()
}
