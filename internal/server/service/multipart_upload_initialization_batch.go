package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadInitializationCandidateError 记录单条分片上传候选的处理错误。
type MultipartUploadInitializationCandidateError struct {
	TaskID uint64
	Err    error
}

// MultipartUploadInitializationBatchResult 汇总本轮扫描、会话绑定、重试和失败。
type MultipartUploadInitializationBatchResult struct {
	Scanned          int
	Bound            int
	RetriesScheduled int
	CandidateErrors  []MultipartUploadInitializationCandidateError
}

// MultipartUploadInitializationBatch 将一次扫描接到逐条初始化入口。
type MultipartUploadInitializationBatch struct {
	list    func(context.Context, int) ([]domain.MultipartUploadInitializationCandidate, error)
	process func(context.Context, domain.MultipartUploadInitializationCandidate, string) (*domain.UploadTask, error)
}

// NewMultipartUploadInitializationBatch 组装候选扫描和单条处理方法。
func NewMultipartUploadInitializationBatch(
	list func(context.Context, int) ([]domain.MultipartUploadInitializationCandidate, error),
	process func(context.Context, domain.MultipartUploadInitializationCandidate, string) (*domain.UploadTask, error),
) *MultipartUploadInitializationBatch {
	if list == nil || process == nil {
		panic("批量分片初始化需要扫描和单条处理方法")
	}
	return &MultipartUploadInitializationBatch{list: list, process: process}
}

// RunOnce 扫描一次并顺序处理候选，扫描失败或取消结束本轮，单条错误随汇总返回。
func (batch *MultipartUploadInitializationBatch) RunOnce(ctx context.Context, limit int, leaseOwner string) (MultipartUploadInitializationBatchResult, error) {
	// 1. 校验上下文和上限，只扫描一次候选。
	var summary MultipartUploadInitializationBatchResult
	if ctx == nil || limit < 1 || limit > domain.MaxMultipartUploadInitializationCandidates {
		return summary, fmt.Errorf("批量分片初始化需要上下文和 1～%d 条的扫描上限", domain.MaxMultipartUploadInitializationCandidates)
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

	// 2. 按扫描顺序逐条处理，记录绑定、重试或失败。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		task, err := batch.process(ctx, candidate, leaseOwner)
		if err == nil {
			switch {
			case task == nil || task.ID != candidate.TaskID || task.UserID != candidate.UserID || task.StagedObjectID != candidate.StagedObjectID ||
				task.UploadMode != "MULTIPART" || task.LeaseVersion == 0 || task.LeaseVersion-1 != candidate.LeaseVersion ||
				task.LeaseOwner != nil || task.LeaseUntil != nil:
				err = domain.ErrInvalidUploadTaskData
			case task.Status == domain.UploadStatusUploading && task.StorageUploadID != nil &&
				domain.ValidMultipartUploadID(*task.StorageUploadID) && task.NextAttemptAt == nil:
				summary.Bound++
			case task.Status == domain.UploadStatusCreating && task.StorageUploadID == nil &&
				task.NextAttemptAt != nil && task.NextAttemptAt.After(task.UpdatedAt):
				summary.RetriesScheduled++
			default:
				err = domain.ErrInvalidUploadTaskData
			}
		}
		if err != nil {
			summary.CandidateErrors = append(summary.CandidateErrors, MultipartUploadInitializationCandidateError{TaskID: candidate.TaskID, Err: err})
		}
	}

	// 3. 保留已处理结果，返回本轮汇总及可能的取消错误。
	return summary, ctx.Err()
}
