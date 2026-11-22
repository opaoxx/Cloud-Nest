package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadSessionCandidateCheck 保存原会话探测和数据库锁内复核结果。
type MultipartUploadSessionCandidateCheck struct {
	Exists   bool
	Database domain.MultipartUploadSessionLockedCheck
}

// Eligible 判断会话存在且通过数据库锁内的收尾条件。
func (check MultipartUploadSessionCandidateCheck) Eligible() bool {
	return check.Exists && check.Database.Eligible()
}

// MultipartUploadSessionCandidateRechecker 在候选筛选后重新观察单条会话。
type MultipartUploadSessionCandidateRechecker struct {
	provider, bucket string
	checkExists      func(context.Context, string, string) (bool, error)
	recheckLocked    func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionLockedCheck, error)
}

// NewMultipartUploadSessionCandidateRechecker 绑定同一存储范围内的会话探测和数据库加锁复核。
func NewMultipartUploadSessionCandidateRechecker(
	provider, bucket string,
	checkExists func(context.Context, string, string) (bool, error),
	recheckLocked func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionLockedCheck, error),
) *MultipartUploadSessionCandidateRechecker {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || checkExists == nil || recheckLocked == nil {
		panic("复核分片会话候选需要存储配置、会话查询及数据库加锁复核")
	}
	return &MultipartUploadSessionCandidateRechecker{provider: provider, bucket: bucket, checkExists: checkExists, recheckLocked: recheckLocked}
}

// Recheck 先探测会话，再用账号事务复核保护状态。会话缺失时直接返回。
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

	// 2. 在事务外探测原存储会话。
	exists, err := rechecker.checkExists(ctx, session.ObjectKey, session.UploadID)
	if stopError := ctx.Err(); stopError != nil {
		return result, stopError
	}
	if err != nil || !exists {
		return result, err
	}

	// 3. 会话存在时加锁复核，用锁内结果判断是否通过。
	databaseCheck, err := rechecker.recheckLocked(ctx, rechecker.provider, rechecker.bucket, target, session, gracePeriod)
	if stopError := ctx.Err(); stopError != nil {
		return result, stopError
	}
	if err != nil {
		return result, err
	}
	return MultipartUploadSessionCandidateCheck{Exists: true, Database: databaseCheck}, nil
}
