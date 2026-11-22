package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ExpiredFileVersionCandidateError 保留未结算历史的归属、历史行 ID 和失败原因。
type ExpiredFileVersionCandidateError struct {
	UserID    uint64
	VersionID uint64
	Err       error
}

// ExpiredFileVersionBatchResult 汇总一轮到期历史的扫描及结算结果。
type ExpiredFileVersionBatchResult struct {
	Scanned         int
	Settled         int
	StaleCandidates int
	CandidateErrors []ExpiredFileVersionCandidateError
}

// ExpiredFileVersionBatch 连接历史候选扫描和单项结算。
type ExpiredFileVersionBatch struct {
	list   func(context.Context, int) ([]domain.FileVersion, error)
	settle func(context.Context, domain.FileVersion) error
}

// NewExpiredFileVersionBatch 绑定一次扫描和单项事务结算入口。
func NewExpiredFileVersionBatch(
	list func(context.Context, int) ([]domain.FileVersion, error),
	settle func(context.Context, domain.FileVersion) error,
) *ExpiredFileVersionBatch {
	if list == nil || settle == nil {
		panic("批量结算到期历史需要扫描和单项结算方法")
	}
	return &ExpiredFileVersionBatch{list: list, settle: settle}
}

// RunOnce 执行一次有上限的扫描，并按原顺序结算候选。
// 扫描失败或上下文结束时返回整轮错误，单项结算失败保留在汇总中。
func (batch *ExpiredFileVersionBatch) RunOnce(ctx context.Context, limit int) (ExpiredFileVersionBatchResult, error) {
	// 1. 校验批量大小，只扫描一次。
	var summary ExpiredFileVersionBatchResult
	if ctx == nil {
		return summary, errors.New("批量结算到期历史需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if limit < 1 || limit > domain.MaxExpiredFileVersionCandidates {
		return summary, fmt.Errorf("到期历史批次需要 1～%d 条的扫描上限", domain.MaxExpiredFileVersionCandidates)
	}
	candidates, err := batch.list(ctx, limit)
	if err != nil {
		return summary, err
	}
	if len(candidates) > limit {
		return summary, domain.ErrInvalidFileVersionData
	}
	summary.Scanned = len(candidates)

	// 2. 按扫描顺序逐条调用单项结算。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		err := batch.settle(ctx, candidate)

		// 3. 汇总成功、失效和失败，取消时保留已处理的结果。
		if stop := ctx.Err(); stop != nil && errors.Is(err, stop) {
			return summary, stop
		}
		switch {
		case err == nil:
			summary.Settled++
		case errors.Is(err, domain.ErrExpiredFileVersionCandidateStale):
			summary.StaleCandidates++
		default:
			summary.CandidateErrors = append(summary.CandidateErrors, ExpiredFileVersionCandidateError{
				UserID: candidate.UserID, VersionID: candidate.ID, Err: err,
			})
		}
	}
	return summary, ctx.Err()
}
