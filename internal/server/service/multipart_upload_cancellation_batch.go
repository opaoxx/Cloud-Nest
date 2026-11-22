package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadCancellationBatchResult 汇总本轮扫描、取消、重试、跳过和逐项错误。
type MultipartUploadCancellationBatchResult struct {
	Scanned          int
	Cancelled        int
	RetriesScheduled int
	Skipped          int
	CandidateErrors  []UploadCancellationCandidateError
}

// MultipartUploadCancellationBatch 按限量扫描结果逐条领取租约并执行取消。
type MultipartUploadCancellationBatch struct {
	list    func(context.Context, int) ([]domain.UploadCancellationCandidate, error)
	claim   func(context.Context, domain.UploadCancellationCandidate, string) (*domain.UploadTask, error)
	execute func(context.Context, *domain.UploadTask) (*domain.UploadTask, error)
}

// NewMultipartUploadCancellationBatch 绑定仓储扫描、领取和单条取消执行。
func NewMultipartUploadCancellationBatch(
	list func(context.Context, int) ([]domain.UploadCancellationCandidate, error),
	claim func(context.Context, domain.UploadCancellationCandidate, string) (*domain.UploadTask, error),
	execute func(context.Context, *domain.UploadTask) (*domain.UploadTask, error),
) *MultipartUploadCancellationBatch {
	if list == nil || claim == nil || execute == nil {
		panic("批量分片取消需要扫描、租约领取和单条执行方法")
	}
	return &MultipartUploadCancellationBatch{list: list, claim: claim, execute: execute}
}

// RunOnce 扫描一次并串行处理候选。扫描错误与停止原因返回给调用方。
func (batch *MultipartUploadCancellationBatch) RunOnce(ctx context.Context, limit int, leaseOwner string) (MultipartUploadCancellationBatchResult, error) {
	// 1. 校验参数，只扫描一次限量候选。
	var summary MultipartUploadCancellationBatchResult
	if ctx == nil || limit < 1 || limit > domain.MaxUploadCancellationCandidates {
		return summary, fmt.Errorf("批量分片取消需要上下文和 1～%d 条的批量上限", domain.MaxUploadCancellationCandidates)
	}
	if len(leaseOwner) == 0 || len(leaseOwner) > 64 || strings.ContainsFunc(leaseOwner, func(character rune) bool { return character > 127 }) {
		return summary, domain.ErrInvalidUploadTaskData
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

	// 2. 逐条领取租约并执行，单条错误留在本轮汇总。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if err := batch.processCandidate(ctx, candidate, leaseOwner, &summary); err != nil {
			if stopError := ctx.Err(); stopError != nil && unwrapSingleUploadError(err) == stopError {
				return summary, stopError
			}
			summary.CandidateErrors = append(summary.CandidateErrors, UploadCancellationCandidateError{TaskID: candidate.TaskID, Err: err})
		}
	}

	// 3. 返回本轮汇总，取消时保留已完成结果。
	return summary, ctx.Err()
}

func (batch *MultipartUploadCancellationBatch) processCandidate(
	ctx context.Context, candidate domain.UploadCancellationCandidate, leaseOwner string, summary *MultipartUploadCancellationBatchResult,
) error {
	// 1. 按原候选领取租约，复核归属、对象和递增后的版本。
	if candidate.TaskID == 0 || candidate.UserID == 0 || candidate.StagedObjectID == 0 {
		return domain.ErrInvalidUploadTaskData
	}
	claimed, err := batch.claim(ctx, candidate, leaseOwner)
	if err != nil {
		if isUploadCandidateStale(err) {
			summary.Skipped++
			return nil
		}
		return err
	}
	if err := validateCancellationStorageClaim(ctx, claimed); err != nil {
		return err
	}
	if claimed.ID != candidate.TaskID || claimed.UserID != candidate.UserID || claimed.StagedObjectID != candidate.StagedObjectID ||
		claimed.LeaseVersion-1 != candidate.LeaseVersion || *claimed.LeaseOwner != leaseOwner {
		return domain.ErrInvalidUploadTaskData
	}

	// 2. 执行单条取消，识别已提交重试和观察后失效的候选。
	finished, err := batch.execute(ctx, claimed)
	if err != nil {
		if finished != nil {
			return errors.Join(domain.ErrInvalidUploadTaskData, err)
		}
		for cause := err; cause != nil; cause = errors.Unwrap(cause) {
			if scheduled, ok := cause.(*MultipartUploadCancellationRetryScheduledError); ok {
				if scheduled == nil || scheduled.NextAttemptAt.IsZero() || scheduled.NextAttemptAt.Before(*claimed.LeaseUntil) {
					return domain.ErrInvalidUploadTaskData
				}
				summary.RetriesScheduled++
				return nil
			}
		}
		if isUploadCandidateStale(err) {
			summary.Skipped++
			return nil
		}
		return err
	}

	// 3. 只对匹配原租约的已提交取消终态计数。
	if !validMultipartCancellationCompletion(finished, claimed) {
		return domain.ErrInvalidUploadTaskData
	}
	summary.Cancelled++
	return nil
}
