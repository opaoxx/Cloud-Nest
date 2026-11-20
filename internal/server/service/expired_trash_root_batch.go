package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ExpiredTrashRootBatchResult 汇总一轮到期根项扫描和标记结果。
type ExpiredTrashRootBatchResult struct {
	Scanned         int
	Marked          int
	StaleCandidates int
	CandidateErrors []PurgeRootCandidateError
}

// ExpiredTrashRootBatch 连接到期根项扫描和单项标记。
type ExpiredTrashRootBatch struct {
	list func(context.Context, int) ([]domain.ExpiredTrashRootCandidate, error)
	mark func(context.Context, domain.ExpiredTrashRootCandidate) (*domain.PurgeRootCandidate, error)
}

// NewExpiredTrashRootBatch 绑定到期扫描和单项标记入口。
func NewExpiredTrashRootBatch(
	list func(context.Context, int) ([]domain.ExpiredTrashRootCandidate, error),
	mark func(context.Context, domain.ExpiredTrashRootCandidate) (*domain.PurgeRootCandidate, error),
) *ExpiredTrashRootBatch {
	// 1. 检查并保存扫描与标记依赖。
	if list == nil || mark == nil {
		panic("批量标记到期回收站根项需要扫描和单项标记方法")
	}
	return &ExpiredTrashRootBatch{list: list, mark: mark}
}

// RunOnce 执行一次限量扫描，按顺序标记候选。
// 单项错误随汇总返回，扫描失败或取消时结束本轮。
func (batch *ExpiredTrashRootBatch) RunOnce(ctx context.Context, limit int) (ExpiredTrashRootBatchResult, error) {
	// 1. 校验上下文和上限，只扫描一次。
	var summary ExpiredTrashRootBatchResult
	if ctx == nil {
		return summary, errors.New("批量标记到期回收站根项需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if limit < 1 || limit > domain.MaxExpiredTrashRootCandidates {
		return summary, fmt.Errorf("到期回收站批次需要 1～%d 条的扫描上限", domain.MaxExpiredTrashRootCandidates)
	}
	candidates, err := batch.list(ctx, limit)
	if err != nil {
		return summary, err
	}
	if len(candidates) > limit {
		return summary, domain.ErrInvalidFileEntryData
	}
	summary.Scanned = len(candidates)

	// 2. 按扫描顺序逐项调用现有条件标记。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		root, err := batch.mark(ctx, candidate)

		// 3. 汇总已标记、已失效和失败结果。
		if err == nil && (root == nil || !root.IsValid() || root.EntryID != candidate.EntryID || root.UserID != candidate.UserID ||
			root.ParentID != candidate.ParentID || root.Revision != candidate.Revision+1) {
			err = domain.ErrInvalidFileEntryData
		}
		switch {
		case err == nil:
			summary.Marked++
		case errors.Is(err, domain.ErrExpiredTrashRootCandidateStale):
			summary.StaleCandidates++
		default:
			summary.CandidateErrors = append(summary.CandidateErrors, PurgeRootCandidateError{
				UserID: candidate.UserID, RootID: candidate.EntryID, Err: err,
			})
		}
	}
	return summary, ctx.Err()
}
