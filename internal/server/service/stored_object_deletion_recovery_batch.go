package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// StoredObjectDeletionCandidateError 保存单个对象的归属及失败原因。
type StoredObjectDeletionCandidateError struct {
	ObjectID    uint64
	OwnerUserID uint64
	Err         error
}

// StoredObjectDeletionRecoveryResult 汇总一次删除恢复扫描与完成结果。
type StoredObjectDeletionRecoveryResult struct {
	Scanned         int
	Completed       int
	CandidateErrors []StoredObjectDeletionCandidateError
}

// StoredObjectDeletionRecoveryBatch 连接删除记录扫描与单项处理。
type StoredObjectDeletionRecoveryBatch struct {
	list    func(context.Context, int) ([]domain.StoredObject, error)
	process func(context.Context, *domain.StoredObject) (*domain.StoredObject, error)
}

// NewStoredObjectDeletionRecoveryBatch 绑定 DELETING 扫描与单对象执行。
func NewStoredObjectDeletionRecoveryBatch(
	list func(context.Context, int) ([]domain.StoredObject, error),
	process func(context.Context, *domain.StoredObject) (*domain.StoredObject, error),
) *StoredObjectDeletionRecoveryBatch {
	// 1. 检查并保存扫描与处理依赖。
	if list == nil || process == nil {
		panic("恢复存储对象删除需要扫描和单项处理方法")
	}
	return &StoredObjectDeletionRecoveryBatch{list: list, process: process}
}

// RunOnce 只扫描一次，逐项处理已提交的 DELETING 记录。
// 扫描或取消错误随汇总返回，单项错误保留在汇总中。
func (batch *StoredObjectDeletionRecoveryBatch) RunOnce(ctx context.Context, limit int) (StoredObjectDeletionRecoveryResult, error) {
	// 1. 校验上下文和上限，扫描一次 DELETING 记录。
	var summary StoredObjectDeletionRecoveryResult
	if ctx == nil {
		return summary, errors.New("恢复存储对象删除需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if limit < 1 || limit > domain.MaxStoredObjectGCCandidates {
		return summary, fmt.Errorf("删除恢复批次需要 1～%d 条的扫描上限", domain.MaxStoredObjectGCCandidates)
	}
	candidates, err := batch.list(ctx, limit)
	if err != nil {
		return summary, err
	}
	if len(candidates) > limit {
		return summary, domain.ErrInvalidStoredObjectData
	}
	summary.Scanned = len(candidates)

	// 2. 按扫描顺序逐项执行删除与写回。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		completed, err := batch.process(ctx, &candidate)
		if err == nil && (completed == nil || completed.ID != candidate.ID || completed.OwnerUserID != candidate.OwnerUserID ||
			completed.Status != domain.StoredObjectStatusDeleted) {
			err = domain.ErrInvalidStoredObjectData
		}

		// 3. 汇总完成数和失败原因，收到取消信号时结束本轮。
		if err != nil {
			summary.CandidateErrors = append(summary.CandidateErrors, StoredObjectDeletionCandidateError{
				ObjectID: candidate.ID, OwnerUserID: candidate.OwnerUserID, Err: err,
			})
			continue
		}
		summary.Completed++
	}
	return summary, ctx.Err()
}
