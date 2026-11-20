package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// StoredObjectGCRoundResult 保存一轮回收中两个批次的结果。
type StoredObjectGCRoundResult struct {
	Recovery StoredObjectDeletionRecoveryResult
	Due      StoredObjectGCBatchResult
}

// StoredObjectGCRound 先恢复删除中的对象，再处理到期候选。
type StoredObjectGCRound struct {
	runRecovery func(context.Context, int) (StoredObjectDeletionRecoveryResult, error)
	runDue      func(context.Context, int) (StoredObjectGCBatchResult, error)
}

// NewStoredObjectGCRound 绑定删除恢复和到期回收两个批次。
func NewStoredObjectGCRound(
	runRecovery func(context.Context, int) (StoredObjectDeletionRecoveryResult, error),
	runDue func(context.Context, int) (StoredObjectGCBatchResult, error),
) *StoredObjectGCRound {
	// 1. 检查并保存两个批次的执行入口。
	if runRecovery == nil || runDue == nil {
		panic("对象回收需要删除恢复和到期处理两个批次")
	}
	return &StoredObjectGCRound{runRecovery: runRecovery, runDue: runDue}
}

// RunOnce 顺序执行两个批次，保留各批次已确认的进度。
func (round *StoredObjectGCRound) RunOnce(ctx context.Context, recoveryLimit, dueLimit int) (StoredObjectGCRoundResult, error) {
	// 1. 校验上下文和两个扫描上限。
	var result StoredObjectGCRoundResult
	if ctx == nil {
		return result, errors.New("执行对象回收需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if recoveryLimit < 1 || recoveryLimit > domain.MaxStoredObjectGCCandidates ||
		dueLimit < 1 || dueLimit > domain.MaxStoredObjectGCCandidates {
		return result, fmt.Errorf("删除恢复和到期处理的扫描上限各需为 1～%d", domain.MaxStoredObjectGCCandidates)
	}

	// 2. 先恢复已提交的删除记录，保留恢复结果。
	recovery, err := round.runRecovery(ctx, recoveryLimit)
	result.Recovery = recovery
	if err != nil {
		return result, fmt.Errorf("执行删除恢复批次失败: %w", err)
	}

	// 3. 再处理新到期候选，返回两段进度及阶段错误。
	if err := ctx.Err(); err != nil {
		return result, err
	}
	due, err := round.runDue(ctx, dueLimit)
	result.Due = due
	if err != nil {
		return result, fmt.Errorf("执行到期对象批次失败: %w", err)
	}
	return result, ctx.Err()
}
