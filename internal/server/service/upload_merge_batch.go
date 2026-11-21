package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadMergeCandidateError 保存本轮未能处理的任务及其原因。
type UploadMergeCandidateError struct {
	TaskID uint64
	Err    error
}

// UploadMergeBatchResult 汇总已提交结果、跳过和单条错误。
type UploadMergeBatchResult struct {
	Scanned          int
	Verifying        int
	Reopened         int
	Failed           int // 已提交 UPLOAD_SESSION_LOST 的终态。
	RetriesScheduled int
	Skipped          int
	CandidateErrors  []UploadMergeCandidateError
}

// UploadMergeBatch 扫描一批候选并逐条领取租约、执行合并。
type UploadMergeBatch struct {
	list    func(context.Context, int) ([]domain.MultipartUploadCompletionCandidate, error)
	claim   func(context.Context, domain.MultipartUploadCompletionCandidate, string) (*domain.UploadTask, error)
	execute func(context.Context, *domain.UploadTask) (*domain.UploadTask, error)
}

// NewUploadMergeBatch 绑定仓储扫描、租约领取和单条合并执行。
func NewUploadMergeBatch(
	list func(context.Context, int) ([]domain.MultipartUploadCompletionCandidate, error),
	claim func(context.Context, domain.MultipartUploadCompletionCandidate, string) (*domain.UploadTask, error),
	execute func(context.Context, *domain.UploadTask) (*domain.UploadTask, error),
) *UploadMergeBatch {
	if list == nil || claim == nil || execute == nil {
		panic("批量分片合并需要候选扫描、租约领取和执行方法")
	}
	return &UploadMergeBatch{list: list, claim: claim, execute: execute}
}

// RunOnce 仅扫描一轮。扫描或取消错误返回给调用方，单条错误留在汇总。
func (batch *UploadMergeBatch) RunOnce(ctx context.Context, limit int, leaseOwner string) (UploadMergeBatchResult, error) {
	// 1. 校验输入，按上限扫描一次合并候选。
	var summary UploadMergeBatchResult
	if ctx == nil || limit < 1 || limit > domain.MaxMultipartUploadCompletionCandidates {
		return summary, fmt.Errorf("批量分片合并需要上下文和 1～%d 条的扫描上限", domain.MaxMultipartUploadCompletionCandidates)
	}
	if len(leaseOwner) == 0 || len(leaseOwner) > 64 || strings.IndexFunc(leaseOwner, func(character rune) bool { return character > 127 }) >= 0 {
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

	// 2. 依序领取和处理。一个候选的错误由本轮记录，其余候选仍可执行。
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if err := batch.processCandidate(ctx, candidate, leaseOwner, &summary); err != nil {
			if contextError := ctx.Err(); contextError != nil && errors.Is(err, contextError) {
				return summary, contextError
			}
			summary.CandidateErrors = append(summary.CandidateErrors, UploadMergeCandidateError{TaskID: candidate.TaskID, Err: err})
		}
	}

	// 3. 汇总已提交结果、跳过和逐项错误，取消时保留已有结果。
	return summary, ctx.Err()
}

func (batch *UploadMergeBatch) processCandidate(
	ctx context.Context, candidate domain.MultipartUploadCompletionCandidate, leaseOwner string, summary *UploadMergeBatchResult,
) error {
	// 1. 领取与候选匹配的新租约，已经被占用或失效的候选计为跳过。
	claimed, err := batch.claim(ctx, candidate, leaseOwner)
	if err != nil {
		if isUploadMergeClaimSkipped(err) {
			summary.Skipped++
			return nil
		}
		return err
	}
	if !validUploadMergeClaim(claimed) || claimed.ID != candidate.TaskID || claimed.UserID != candidate.UserID ||
		claimed.StagedObjectID != candidate.StagedObjectID || claimed.LeaseVersion-1 != candidate.LeaseVersion ||
		*claimed.LeaseOwner != leaseOwner {
		return domain.ErrInvalidUploadTaskData
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. 使用已提交的租约执行，只有已登记的重试才计入重试数。
	task, err := batch.execute(ctx, claimed)
	if err != nil {
		var scheduled *UploadMergeRetryScheduledError
		if task == nil && errors.As(err, &scheduled) && scheduled != nil && !scheduled.NextAttemptAt.IsZero() {
			summary.RetriesScheduled++
			return nil
		}
		return err
	}

	// 3. 按同一任务、会话和租约版本的已提交状态分类。
	if task == nil || !sameMergeExecutionBinding(task, claimed) || task.LeaseOwner != nil || task.LeaseUntil != nil || task.NextAttemptAt != nil {
		return domain.ErrInvalidUploadTaskData
	}
	switch task.Status {
	case domain.UploadStatusVerifying:
		summary.Verifying++
	case domain.UploadStatusUploading:
		summary.Reopened++
	case domain.UploadStatusFailed:
		if task.LastErrorCode == nil || *task.LastErrorCode != domain.UploadSessionLostCode || task.ReservedBytes != 0 || task.CompletedAt == nil {
			return domain.ErrInvalidUploadTaskData
		}
		summary.Failed++
	default:
		return domain.ErrInvalidUploadTaskData
	}
	return nil
}

func isUploadMergeClaimSkipped(err error) bool {
	// 只对单一的候选失效原因计跳过，保留可能含回滚错误的合并错误。
	for cause := errors.Unwrap(err); cause != nil; cause = errors.Unwrap(err) {
		err = cause
	}
	return err == domain.ErrUploadTaskNotFound || err == domain.ErrUploadTaskNotReady || err == domain.ErrUploadTaskBusy
}
