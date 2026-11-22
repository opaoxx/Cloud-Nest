package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadSessionCleanupLimits 限制每轮的对象、存储页和收尾调用。
type MultipartUploadSessionCleanupLimits struct {
	Objects, SessionsPerPage, StoragePages, Candidates int
}

type multipartUploadSessionCleanupCandidate struct {
	target  domain.MultipartUploadSessionScanTarget
	session domain.MultipartUploadSession
}

// MultipartUploadSessionCleanupPosition 保存用于下一轮的进程内位置。零值表示从头扫。
type MultipartUploadSessionCleanupPosition struct {
	cursor  MultipartUploadSessionCandidateCursor
	pending []multipartUploadSessionCleanupCandidate
	atEnd   bool
}

// MultipartUploadSessionCleanupItem 保存单条收尾的归属、观察和错误。
type MultipartUploadSessionCleanupItem struct {
	Target  domain.MultipartUploadSessionScanTarget
	Session domain.MultipartUploadSession
	Result  MultipartUploadSessionCandidateCleanupResult
	Err     error
}

// MultipartUploadSessionCleanupRoundResult 汇总本轮扫描、收尾及续扫位置。
type MultipartUploadSessionCleanupRoundResult struct {
	Scan                MultipartUploadSessionCandidateBatch
	Items               []MultipartUploadSessionCleanupItem
	Skipped, Confirmed  int
	Unconfirmed, Failed int
	Next                MultipartUploadSessionCleanupPosition
	Done                bool
	ScanRestarted       bool
}

// MultipartUploadSessionCleanupRound 串行处理跨对象扫描取得的会话候选。
type MultipartUploadSessionCleanupRound struct {
	scanner *MultipartUploadSessionCandidateBatchScanner
	cleanup *MultipartUploadSessionCandidateCleanup
}

// NewMultipartUploadSessionCleanupRound 绑定同一存储范围的扫描和单条收尾。
func NewMultipartUploadSessionCleanupRound(
	scanner *MultipartUploadSessionCandidateBatchScanner, cleanup *MultipartUploadSessionCandidateCleanup,
) *MultipartUploadSessionCleanupRound {
	if scanner == nil || cleanup == nil {
		panic("分片会话单轮收尾需要候选扫描和单条收尾")
	}
	return &MultipartUploadSessionCleanupRound{scanner: scanner, cleanup: cleanup}
}

// RunOnce 最多扫描一个对象页，并处理限量候选。逐项错误留在汇总，扫描失败或取消结束本轮。
func (round *MultipartUploadSessionCleanupRound) RunOnce(
	ctx context.Context, start MultipartUploadSessionCleanupPosition, limits MultipartUploadSessionCleanupLimits, gracePeriod time.Duration,
) (MultipartUploadSessionCleanupRoundResult, error) {
	// 1. 恢复未处理候选，或读取一批有上限的候选。
	result := MultipartUploadSessionCleanupRoundResult{Next: start}
	if ctx == nil || gracePeriod <= 0 || limits.Objects < 1 || limits.SessionsPerPage < 1 ||
		limits.SessionsPerPage > domain.MaxMultipartUploadSessionsPageSize || limits.StoragePages < 1 || limits.Candidates < 1 {
		return result, fmt.Errorf("分片会话单轮收尾需要上下文、有效批量上限和正数宽限期")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(start.pending) == 0 && !start.atEnd {
		scan, err := round.scanner.ScanObjectPage(ctx, start.cursor, limits.Objects, limits.SessionsPerPage, limits.StoragePages, gracePeriod)
		result.Scan = scan
		if errors.Is(err, domain.ErrMultipartUploadSessionMarkerMissing) {
			// 该批尚未处理。回退起始游标，避免跨过失败页之前尚未处理的候选。
			if start.cursor.UploadID != "" {
				result.Next.cursor = MultipartUploadSessionCandidateCursor{ObjectID: start.cursor.ObjectID - 1}
			}
			result.ScanRestarted = true
			return result, nil
		}
		if err != nil {
			return result, err
		}
		result.Next.cursor, result.Next.atEnd = scan.Next, scan.Done
		result.Next.pending = nil
		for _, object := range scan.Objects {
			for _, session := range object.Scan.Candidates {
				result.Next.pending = append(result.Next.pending, multipartUploadSessionCleanupCandidate{target: object.Target, session: session})
			}
		}
	}

	// 2. 按顺序处理至本轮上限，并记录逐条结果。
	for len(result.Next.pending) > 0 && len(result.Items) < limits.Candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		candidate := result.Next.pending[0]
		checked, err := round.cleanup.Execute(ctx, candidate.target, candidate.session, gracePeriod)
		result.Items = append(result.Items, MultipartUploadSessionCleanupItem{
			Target: candidate.target, Session: candidate.session, Result: checked, Err: err,
		})
		if err != nil {
			result.Failed++
		}
		switch {
		case checked.AbortOutcome == MultipartUploadSessionAbortSkipped:
			result.Skipped++
		case checked.Observation != nil:
			result.Confirmed++
		case checked.AbortOutcome != "":
			result.Unconfirmed++
		}

		// 3. 推进已处理候选。取消时保留当前项，下一轮重新复核。
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Next.pending = result.Next.pending[1:]
	}
	result.Done = result.Next.atEnd && len(result.Next.pending) == 0
	return result, nil
}
