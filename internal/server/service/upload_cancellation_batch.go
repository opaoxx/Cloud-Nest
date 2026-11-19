package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadCancellationCandidateError 记录单条取消候选的处理错误。
type UploadCancellationCandidateError struct {
	TaskID uint64
	Err    error
}

// UploadCancellationBatchResult 汇总扫描数、已确认的取消终态和逐条错误。
type UploadCancellationBatchResult struct {
	Scanned         int
	Cancelled       int
	CandidateErrors []UploadCancellationCandidateError
}

// UploadCancellationBatch 将限量扫描接到顺序执行的单条取消收尾。
type UploadCancellationBatch struct {
	list    func(context.Context, int) ([]domain.UploadCancellationCandidate, error)
	process func(context.Context, domain.UploadCancellationCandidate) (*domain.UploadTask, error)
}

// NewUploadCancellationBatch 绑定一次扫描和单条候选处理。
func NewUploadCancellationBatch(
	list func(context.Context, int) ([]domain.UploadCancellationCandidate, error),
	process func(context.Context, domain.UploadCancellationCandidate) (*domain.UploadTask, error),
) *UploadCancellationBatch {
	// 1. 校验两项依赖并保存，供每轮调用。
	if list == nil || process == nil {
		panic("批量上传取消收尾需要扫描和候选处理方法")
	}
	return &UploadCancellationBatch{list: list, process: process}
}

// RunOnce 扫描一次并顺序处理单文件取消候选。
// 扫描和上下文错误直接返回，单条处理错误保存在本轮汇总中。
func (batch *UploadCancellationBatch) RunOnce(ctx context.Context, limit int) (UploadCancellationBatchResult, error) {
	// 1. 核对调用状态和上限，只扫描一次候选。
	var summary UploadCancellationBatchResult
	if ctx == nil || limit < 1 || limit > domain.MaxUploadCancellationCandidates {
		return summary, fmt.Errorf("批量上传取消收尾需要上下文和 1～%d 条的批量大小", domain.MaxUploadCancellationCandidates)
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

	// 2. 按扫描顺序调用单条处理，确认终态或记录错误。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		task, err := batch.process(ctx, candidate)
		if err == nil && (task == nil || task.ID != candidate.TaskID || task.UserID != candidate.UserID ||
			task.StagedObjectID != candidate.StagedObjectID || task.LeaseVersion != candidate.LeaseVersion ||
			task.UploadMode != "SINGLE" || task.Status != domain.UploadStatusCancelled ||
			task.ReservedBytes != 0 || task.CompletedAt == nil) {
			err = domain.ErrInvalidUploadTaskData
		}
		if err != nil {
			summary.CandidateErrors = append(summary.CandidateErrors, UploadCancellationCandidateError{TaskID: candidate.TaskID, Err: err})
			continue
		}
		summary.Cancelled++
	}

	// 3. 返回本轮汇总及可能的停止原因。
	return summary, ctx.Err()
}
