package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// StoredObjectGCBatchResult 汇总一次到期候选扫描和处理结果。
type StoredObjectGCBatchResult struct {
	Scanned         int
	Completed       int
	StaleCandidates int
	CandidateErrors []StoredObjectDeletionCandidateError
}

// StoredObjectGCBatch 连接到期候选扫描和单项领取执行。
type StoredObjectGCBatch struct {
	list    func(context.Context, int) ([]domain.StoredObjectGCCandidate, error)
	process func(context.Context, domain.StoredObjectGCCandidate) (*domain.StoredObject, error)
}

// NewStoredObjectGCBatch 绑定扫描和单项处理入口。
func NewStoredObjectGCBatch(
	list func(context.Context, int) ([]domain.StoredObjectGCCandidate, error),
	process func(context.Context, domain.StoredObjectGCCandidate) (*domain.StoredObject, error),
) *StoredObjectGCBatch {
	// 1. 检查并保存扫描与处理依赖。
	if list == nil || process == nil {
		panic("批量回收存储对象需要扫描和单项处理方法")
	}
	return &StoredObjectGCBatch{list: list, process: process}
}

// RunOnce 扫描一次到期候选并顺序执行。
// 扫描或取消错误随汇总返回，单项错误保留在汇总中。
func (batch *StoredObjectGCBatch) RunOnce(ctx context.Context, limit int) (StoredObjectGCBatchResult, error) {
	// 1. 校验上下文和上限，只扫描一次到期候选。
	var summary StoredObjectGCBatchResult
	if ctx == nil {
		return summary, errors.New("批量回收存储对象需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if limit < 1 || limit > domain.MaxStoredObjectGCCandidates {
		return summary, fmt.Errorf("对象回收批次需要 1～%d 条的扫描上限", domain.MaxStoredObjectGCCandidates)
	}
	candidates, err := batch.list(ctx, limit)
	if err != nil {
		return summary, err
	}
	if len(candidates) > limit {
		return summary, domain.ErrInvalidStoredObjectData
	}
	summary.Scanned = len(candidates)

	// 2. 按扫描顺序逐项领取并执行删除。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		completed, err := batch.process(ctx, candidate)

		// 3. 汇总完成、失效和失败，取消时保留已完成进度。
		if err == nil && (completed == nil || completed.ID != candidate.ObjectID ||
			completed.OwnerUserID != candidate.OwnerUserID || completed.Status != domain.StoredObjectStatusDeleted) {
			err = domain.ErrInvalidStoredObjectData
		}
		switch {
		case err == nil:
			summary.Completed++
		case errors.Is(err, domain.ErrStoredObjectGCCandidateStale):
			summary.StaleCandidates++
		default:
			summary.CandidateErrors = append(summary.CandidateErrors, StoredObjectDeletionCandidateError{
				ObjectID: candidate.ObjectID, OwnerUserID: candidate.OwnerUserID, Err: err,
			})
		}
	}
	return summary, ctx.Err()
}
