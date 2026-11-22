package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadSessionScanFailure 记录单个会话的仓储复核错误。
type MultipartUploadSessionScanFailure struct {
	UploadID string
	Err      error
}

// MultipartUploadSessionCandidatePage 汇总同一对象的一页待复核会话。
type MultipartUploadSessionCandidatePage struct {
	Scanned            int
	Candidates         []domain.MultipartUploadSession
	SkippedBound       int
	SkippedCreation    int
	SkippedWithinGrace int
	Failures           []MultipartUploadSessionScanFailure
	// NextUploadID 只在整页正常返回时可用于继续读取。
	NextUploadID string
}

// MultipartUploadSessionCandidateScanner 组合一个对象的会话分页和数据库只读复核。
type MultipartUploadSessionCandidateScanner struct {
	provider, bucket string
	listPage         func(context.Context, string, string, int) (*domain.MultipartUploadSessionPage, error)
	readFacts        func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionCleanupFacts, error)
}

// NewMultipartUploadSessionCandidateScanner 绑定同一存储配置的分页和复核操作。
func NewMultipartUploadSessionCandidateScanner(
	provider, bucket string,
	listPage func(context.Context, string, string, int) (*domain.MultipartUploadSessionPage, error),
	readFacts func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionCleanupFacts, error),
) *MultipartUploadSessionCandidateScanner {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || listPage == nil || readFacts == nil {
		panic("扫描分片会话候选需要存储配置、分页及数据库复核")
	}
	return &MultipartUploadSessionCandidateScanner{provider: provider, bucket: bucket, listPage: listPage, readFacts: readFacts}
}

// ScanPage 只读取一页并筛选待进一步复核的会话。
// 逐项错误进入汇总。取消时返回部分结果，游标保持未交付。
func (scanner *MultipartUploadSessionCandidateScanner) ScanPage(
	ctx context.Context, target domain.MultipartUploadSessionScanTarget, marker string, limit int, gracePeriod time.Duration,
) (MultipartUploadSessionCandidatePage, error) {
	// 1. 校验对象范围、宽限期和分页参数。
	var result MultipartUploadSessionCandidatePage
	if ctx == nil || gracePeriod <= 0 || limit < 1 || limit > domain.MaxMultipartUploadSessionsPageSize {
		return result, fmt.Errorf("筛选分片会话需要上下文、有效页大小和正数宽限期")
	}
	if target.TaskID == 0 || target.ObjectID == 0 || (marker != "" && !domain.ValidMultipartUploadID(marker)) {
		return result, domain.ErrInvalidUploadTaskData
	}
	if owner, valid := domain.ParseUploadObjectKey(target.ObjectKey); !valid || owner != target.UserID {
		return result, domain.ErrInvalidStoredObjectData
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	// 2. 读取一页会话，并逐条复核当前保护事实。
	page, err := scanner.listPage(ctx, target.ObjectKey, marker, limit)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !validMultipartUploadSessionCandidatePage(page, target.ObjectKey, marker, limit) {
		return result, domain.ErrUploadStorageUnavailable
	}
	result.Scanned = len(page.Sessions)
	for _, session := range page.Sessions {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		facts, err := scanner.readFacts(ctx, scanner.provider, scanner.bucket, target, session, gracePeriod)
		if err != nil {
			if stopped := ctx.Err(); stopped != nil && unwrapSingleUploadError(err) == stopped {
				return result, stopped
			}
			result.Failures = append(result.Failures, MultipartUploadSessionScanFailure{UploadID: session.UploadID, Err: err})
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		switch {
		case facts.Protection.Bound:
			result.SkippedBound++
		case facts.Protection.ActiveCreationLease:
			result.SkippedCreation++
		case !facts.GraceElapsed:
			result.SkippedWithinGrace++
		default:
			result.Candidates = append(result.Candidates, session)
		}
	}

	// 3. 仅在整页复核完成后交付存储游标。
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.NextUploadID = page.NextUploadID
	return result, nil
}

func validMultipartUploadSessionCandidatePage(page *domain.MultipartUploadSessionPage, objectKey, marker string, limit int) bool {
	if page == nil || len(page.Sessions) > limit {
		return false
	}
	seen := make(map[string]struct{}, len(page.Sessions))
	for _, session := range page.Sessions {
		_, duplicate := seen[session.UploadID]
		if session.ObjectKey != objectKey || !domain.ValidMultipartUploadID(session.UploadID) ||
			session.UploadID == marker || duplicate || session.InitiatedAt.IsZero() {
			return false
		}
		seen[session.UploadID] = struct{}{}
	}
	return page.NextUploadID == "" || (len(page.Sessions) > 0 && page.NextUploadID == page.Sessions[len(page.Sessions)-1].UploadID)
}
