package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadExpirationBatchResult 汇总本轮扫描、到期、重试、跳过和逐项错误。
type MultipartUploadExpirationBatchResult struct {
	Scanned          int
	Expired          int
	RetriesScheduled int
	Skipped          int
	CandidateErrors  []UploadExpirationCandidateError
}

// MultipartUploadExpirationBatch 按扫描顺序逐条处理到期的 Multipart 任务。
type MultipartUploadExpirationBatch struct {
	list    func(context.Context, int) ([]domain.UploadExpirationCandidate, error)
	claim   func(context.Context, domain.UploadExpirationCandidate, string) (*domain.UploadTask, error)
	execute func(context.Context, *domain.UploadTask) (*domain.UploadTask, error)
}

// NewMultipartUploadExpirationBatch 绑定正式扫描、租约领取和单条到期执行。
func NewMultipartUploadExpirationBatch(
	list func(context.Context, int) ([]domain.UploadExpirationCandidate, error),
	claim func(context.Context, domain.UploadExpirationCandidate, string) (*domain.UploadTask, error),
	execute func(context.Context, *domain.UploadTask) (*domain.UploadTask, error),
) *MultipartUploadExpirationBatch {
	if list == nil || claim == nil || execute == nil {
		panic("批量分片到期需要扫描、租约领取和单条执行方法")
	}
	return &MultipartUploadExpirationBatch{list: list, claim: claim, execute: execute}
}

// RunOnce 扫描一次并串行处理。扫描及停止错误直接返回，单条错误留在汇总中。
func (batch *MultipartUploadExpirationBatch) RunOnce(ctx context.Context, limit int, leaseOwner string) (MultipartUploadExpirationBatchResult, error) {
	// 1. 校验参数，只扫描一次限量候选。
	var summary MultipartUploadExpirationBatchResult
	if ctx == nil || limit < 1 || limit > domain.MaxUploadExpirationCandidates {
		return summary, fmt.Errorf("批量分片到期需要上下文和 1～%d 条的批量上限", domain.MaxUploadExpirationCandidates)
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

	// 2. 逐条领取，核对租约并执行单项收尾。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if err := batch.processCandidate(ctx, candidate, leaseOwner, &summary); err != nil {
			if stopError := ctx.Err(); stopError != nil && unwrapSingleUploadError(err) == stopError {
				return summary, stopError
			}
			summary.CandidateErrors = append(summary.CandidateErrors, UploadExpirationCandidateError{TaskID: candidate.TaskID, Err: err})
		}
	}

	// 3. 汇总终态、重试、失效和失败，保留已提交的结果。
	return summary, ctx.Err()
}

func (batch *MultipartUploadExpirationBatch) processCandidate(
	ctx context.Context, candidate domain.UploadExpirationCandidate, leaseOwner string, summary *MultipartUploadExpirationBatchResult,
) error {
	// 1. 领取候选，核对归属、到期时间和新的租约。
	if candidate.TaskID == 0 || candidate.UserID == 0 || candidate.StagedObjectID == 0 ||
		candidate.UploadMode != "MULTIPART" || candidate.ExpiresAt.IsZero() {
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
	if err := validateMultipartCleanupStorageClaim(ctx, claimed, domain.UploadStatusExpiring); err != nil {
		return err
	}
	if claimed.ID != candidate.TaskID || claimed.UserID != candidate.UserID || claimed.StagedObjectID != candidate.StagedObjectID ||
		!claimed.ExpiresAt.Equal(candidate.ExpiresAt) || claimed.LeaseVersion-1 != candidate.LeaseVersion || *claimed.LeaseOwner != leaseOwner {
		return domain.ErrInvalidUploadTaskData
	}

	// 2. 执行单条到期，识别已提交的重试或执行时发现的失效候选。
	finished, err := batch.execute(ctx, claimed)
	if err != nil {
		if finished != nil {
			return errors.Join(domain.ErrInvalidUploadTaskData, err)
		}
		// 只沿单条错误链确认重试，保留包含独立故障的合并错误。
		for cause := err; cause != nil; cause = errors.Unwrap(cause) {
			if scheduled, ok := cause.(*MultipartUploadExpirationRetryScheduledError); ok {
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

	// 3. 只统计同一租约已经提交的 EXPIRED 结果。
	if !validMultipartCleanupCompletion(finished, claimed, domain.UploadStatusExpired) {
		return domain.ErrInvalidUploadTaskData
	}
	summary.Expired++
	return nil
}
