package service

import (
	"context"
	"errors"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadSessionCandidateCleanupResult 保存原中止应答及后续存储观察。
type MultipartUploadSessionCandidateCleanupResult struct {
	AbortOutcome MultipartUploadSessionAbortOutcome
	// Observation 为 nil 表示尚未确认原会话已关闭。
	Observation *domain.MultipartUploadAbortObservation
}

// MultipartUploadSessionCandidateCleanup 串联单条会话的中止与只读复查。
type MultipartUploadSessionCandidateCleanup struct {
	aborter *MultipartUploadSessionCandidateAborter
	observe func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error)
}

// NewMultipartUploadSessionCandidateCleanup 绑定同一存储范围的中止及受信实例复查。
func NewMultipartUploadSessionCandidateCleanup(
	aborter *MultipartUploadSessionCandidateAborter,
	observe func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error),
) *MultipartUploadSessionCandidateCleanup {
	if aborter == nil || observe == nil {
		panic("分片会话收尾需要候选中止入口和存储复查")
	}
	return &MultipartUploadSessionCandidateCleanup{aborter: aborter, observe: observe}
}

// Execute 执行一条候选，并在上下文仍有效时复查中止效果。
// 复查同时保留可确认的存储事实和原请求错误，供调用方判断本轮结果。
func (cleanup *MultipartUploadSessionCandidateCleanup) Execute(
	ctx context.Context, target domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, gracePeriod time.Duration,
) (MultipartUploadSessionCandidateCleanupResult, error) {
	// 1. 执行单条候选中止，保留原始应答。
	outcome, abortError := cleanup.aborter.AbortCandidate(ctx, target, session, gracePeriod)
	result := MultipartUploadSessionCandidateCleanupResult{AbortOutcome: outcome}
	switch outcome {
	case MultipartUploadSessionAbortAccepted, MultipartUploadSessionAbortMissing, MultipartUploadSessionAbortUnconfirmed:
	default:
		return result, abortError
	}

	// 2. 对需要确认的结果只读查询原会话和对象。
	if stopError := ctx.Err(); stopError != nil {
		if abortError == nil {
			return result, stopError
		}
		if errors.Is(abortError, stopError) {
			return result, abortError
		}
		return result, errors.Join(abortError, stopError)
	}
	observation, readError := cleanup.observe(ctx, session.ObjectKey, session.UploadID)

	// 3. 交付有效观察，保留中止和复查阶段各自的错误。
	if stopError := ctx.Err(); stopError != nil {
		switch {
		case readError == nil:
			readError = stopError
		case !errors.Is(readError, stopError):
			readError = errors.Join(readError, stopError)
		}
	} else if readError != nil {
		readError = multipartCleanupStorageFailure(readError)
	}
	if readError != nil {
		if abortError != nil {
			readError = errors.Join(abortError, readError)
		}
		return result, readError
	}
	result.Observation = observation
	return result, abortError
}
