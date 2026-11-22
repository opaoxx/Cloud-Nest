package service

import (
	"context"
	"errors"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadSessionAbortOutcome 保存单条候选的跳过、缺失或中止应答。
type MultipartUploadSessionAbortOutcome string

const (
	MultipartUploadSessionAbortSkipped     MultipartUploadSessionAbortOutcome = "SKIPPED"
	MultipartUploadSessionAbortMissing     MultipartUploadSessionAbortOutcome = "MISSING"
	MultipartUploadSessionAbortAccepted    MultipartUploadSessionAbortOutcome = "ACCEPTED"
	MultipartUploadSessionAbortUnconfirmed MultipartUploadSessionAbortOutcome = "UNCONFIRMED"
)

// MultipartUploadSessionCandidateAborter 在复核后中止一条未关联的会话。
type MultipartUploadSessionCandidateAborter struct {
	rechecker *MultipartUploadSessionCandidateRechecker
	abort     func(context.Context, string, string) error
}

// NewMultipartUploadSessionCandidateAborter 绑定同一桶的复核入口和中止操作。
func NewMultipartUploadSessionCandidateAborter(
	rechecker *MultipartUploadSessionCandidateRechecker, abort func(context.Context, string, string) error,
) *MultipartUploadSessionCandidateAborter {
	if rechecker == nil || abort == nil {
		panic("中止分片会话候选需要复核入口和存储中止操作")
	}
	return &MultipartUploadSessionCandidateAborter{rechecker: rechecker, abort: abort}
}

// AbortCandidate 从最新的复核结果决定是否向原会话发送中止请求。
// 存储应答仍需后续复查。中止前失败时结果为空，中止调用失败时保留未确认结果和错误。
func (aborter *MultipartUploadSessionCandidateAborter) AbortCandidate(
	ctx context.Context, target domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, gracePeriod time.Duration,
) (MultipartUploadSessionAbortOutcome, error) {
	// 1. 重新复核单条候选，跳过缺失或仍受保护的会话。
	check, err := aborter.rechecker.Recheck(ctx, target, session, gracePeriod)
	if err != nil {
		return "", err
	}
	if !check.Exists {
		return MultipartUploadSessionAbortMissing, nil
	}
	if !check.Eligible() {
		return MultipartUploadSessionAbortSkipped, nil
	}

	// 2. 账号事务已结束。保留原对象键和原始会话 ID，发出一次中止调用。
	if err := ctx.Err(); err != nil {
		return "", err
	}
	abortError := aborter.abort(ctx, session.ObjectKey, session.UploadID)

	// 3. 区分存储已受理、会话已缺失和结果未确认。
	if stopError := ctx.Err(); stopError != nil {
		if abortError == nil {
			return MultipartUploadSessionAbortUnconfirmed, stopError
		}
		if !errors.Is(abortError, stopError) {
			abortError = errors.Join(abortError, stopError)
		}
		return MultipartUploadSessionAbortUnconfirmed, abortError
	}
	switch {
	case abortError == nil:
		return MultipartUploadSessionAbortAccepted, nil
	case errors.Is(abortError, domain.ErrUploadPartSessionMissing):
		return MultipartUploadSessionAbortMissing, nil
	default:
		return MultipartUploadSessionAbortUnconfirmed, multipartCleanupStorageFailure(abortError)
	}
}
