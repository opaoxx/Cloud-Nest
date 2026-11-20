package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PurgeRootCandidateError 记录单个清理根项的处理失败原因。
type PurgeRootCandidateError struct {
	UserID uint64
	RootID uint64
	Err    error
}

// PurgeRootBatchResult 汇总一次根项扫描产生的清理进度。
type PurgeRootBatchResult struct {
	Scanned         int
	CompletedLeaves int
	StaleRoots      int
	CandidateErrors []PurgeRootCandidateError
}

// PurgeRootBatch 连接跨用户根项扫描与单根项事务。
type PurgeRootBatch struct {
	list    func(context.Context, int) ([]domain.PurgeRootCandidate, error)
	process func(context.Context, domain.PurgeRootCandidate, int) (int, error)
}

// NewPurgeRootBatch 绑定扫描方法和单根项结算方法。
func NewPurgeRootBatch(
	list func(context.Context, int) ([]domain.PurgeRootCandidate, error),
	process func(context.Context, domain.PurgeRootCandidate, int) (int, error),
) *PurgeRootBatch {
	// 1. 检查扫描与结算依赖。
	if list == nil || process == nil {
		panic("永久清理批次需要根项扫描和单根结算方法")
	}
	return &PurgeRootBatch{list: list, process: process}
}

// RunOnce 扫描一次，逐根提交并汇总结果。
// 扫描与取消错误随结果返回，逐根结算错误记录在结果中。
func (batch *PurgeRootBatch) RunOnce(ctx context.Context, rootLimit, leafLimit int) (PurgeRootBatchResult, error) {
	// 1. 校验上下文和两个上限，扫描一次待清理根项。
	var summary PurgeRootBatchResult
	if ctx == nil {
		return summary, errors.New("执行一轮永久清理需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if rootLimit < 1 || rootLimit > domain.MaxPurgeRootCandidates || leafLimit < 1 || leafLimit > domain.MaxPurgeLeafCandidates {
		return summary, fmt.Errorf("永久清理每轮需要扫描 1～%d 个根项，每个根项结算 1～%d 个叶子",
			domain.MaxPurgeRootCandidates, domain.MaxPurgeLeafCandidates)
	}
	candidates, err := batch.list(ctx, rootLimit)
	if err != nil {
		return summary, err
	}
	if len(candidates) > rootLimit {
		return summary, domain.ErrInvalidFileEntryData
	}
	summary.Scanned = len(candidates)

	// 2. 按扫描顺序为每个根项执行一批结算。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		completed, err := batch.process(ctx, candidate, leafLimit)

		// 3. 统计已提交的叶子，记录失效候选和逐根错误。
		if err == nil && (completed < 0 || completed > leafLimit) {
			err = domain.ErrInvalidFileEntryData
		}
		switch {
		case err == nil:
			summary.CompletedLeaves += completed
		case errors.Is(err, domain.ErrPurgeRootCandidateStale):
			summary.StaleRoots++
		default:
			summary.CandidateErrors = append(summary.CandidateErrors, PurgeRootCandidateError{
				UserID: candidate.UserID, RootID: candidate.EntryID, Err: err,
			})
		}
	}

	// 4. 返回本轮结果，取消时保留已经提交的进度。
	return summary, ctx.Err()
}
