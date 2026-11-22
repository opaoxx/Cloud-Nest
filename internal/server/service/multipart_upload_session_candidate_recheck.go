package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadSessionCandidateCheck 保存原会话及数据库保护的最新只读观察。
type MultipartUploadSessionCandidateCheck struct {
	Exists bool
	Facts  domain.MultipartUploadSessionCleanupFacts
}

// Eligible 判断本次观察中，会话是否仍存在、已过宽限期且未受保护。
// 调用方执行后续操作前仍须协调可能并发的绑定和创建。
func (check MultipartUploadSessionCandidateCheck) Eligible() bool {
	return check.Exists && check.Facts.GraceElapsed && !check.Facts.Protection.Bound && !check.Facts.Protection.ActiveCreationLease
}

// MultipartUploadSessionCandidateRechecker 在候选筛选后重新观察单条会话。
type MultipartUploadSessionCandidateRechecker struct {
	provider, bucket string
	checkExists      func(context.Context, string, string) (bool, error)
	readFacts        func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionCleanupFacts, error)
}

// NewMultipartUploadSessionCandidateRechecker 绑定同一存储范围内的会话探测和数据库复核。
func NewMultipartUploadSessionCandidateRechecker(
	provider, bucket string,
	checkExists func(context.Context, string, string) (bool, error),
	readFacts func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionCleanupFacts, error),
) *MultipartUploadSessionCandidateRechecker {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || checkExists == nil || readFacts == nil {
		panic("复核分片会话候选需要存储配置、会话查询及数据库复核")
	}
	return &MultipartUploadSessionCandidateRechecker{provider: provider, bucket: bucket, checkExists: checkExists, readFacts: readFacts}
}

// Recheck 读取候选当前的会话和保护状态。会话缺失时直接返回，读取失败时返回错误。
func (rechecker *MultipartUploadSessionCandidateRechecker) Recheck(
	ctx context.Context, target domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, gracePeriod time.Duration,
) (MultipartUploadSessionCandidateCheck, error) {
	// 1. 校验候选会话的对象归属和参数。
	var result MultipartUploadSessionCandidateCheck
	if ctx == nil || gracePeriod <= 0 {
		return result, fmt.Errorf("复核分片会话需要上下文和正数宽限期")
	}
	if target.TaskID == 0 || target.ObjectID == 0 || !domain.ValidMultipartUploadID(session.UploadID) {
		return result, domain.ErrInvalidUploadTaskData
	}
	if owner, valid := domain.ParseUploadObjectKey(target.ObjectKey); !valid || owner != target.UserID ||
		session.ObjectKey != target.ObjectKey || session.InitiatedAt.IsZero() {
		return result, domain.ErrInvalidStoredObjectData
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	// 2. 只读确认原存储会话当前是否存在。
	exists, err := rechecker.checkExists(ctx, session.ObjectKey, session.UploadID)
	if stopError := ctx.Err(); stopError != nil {
		return result, stopError
	}
	if err != nil || !exists {
		return result, err
	}

	// 3. 刷新数据库保护和宽限期，返回复核结果。
	facts, err := rechecker.readFacts(ctx, rechecker.provider, rechecker.bucket, target, session, gracePeriod)
	if stopError := ctx.Err(); stopError != nil {
		return result, stopError
	}
	if err != nil {
		return result, err
	}
	return MultipartUploadSessionCandidateCheck{Exists: true, Facts: facts}, nil
}
