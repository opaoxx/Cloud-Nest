package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadSessionCandidateCursor 定位跨对象扫描的续读位置。
// UploadID 为空时从 ObjectID 之后读起。非空时尝试续读该对象的会话。
type MultipartUploadSessionCandidateCursor struct {
	ObjectID uint64
	UploadID string
}

// MultipartUploadSessionObjectScan 保存同一原对象的目标和会话复核结果。
type MultipartUploadSessionObjectScan struct {
	Target domain.MultipartUploadSessionScanTarget
	Scan   MultipartUploadSessionCandidateScan
}

// MultipartUploadSessionCandidateBatch 汇总一页原对象范围内的扫描。
type MultipartUploadSessionCandidateBatch struct {
	Objects      []MultipartUploadSessionObjectScan
	PagesScanned int
	Next         MultipartUploadSessionCandidateCursor
	Done         bool
}

// MultipartUploadSessionCandidateBatchScanner 只读串联原对象和会话分页。
type MultipartUploadSessionCandidateBatchScanner struct {
	scanner     *MultipartUploadSessionCandidateScanner
	listTargets func(context.Context, string, string, uint64, int) (*domain.MultipartUploadSessionScanPage, error)
}

// NewMultipartUploadSessionCandidateBatchScanner 使用单对象扫描的存储范围查找原对象。
func NewMultipartUploadSessionCandidateBatchScanner(
	scanner *MultipartUploadSessionCandidateScanner,
	listTargets func(context.Context, string, string, uint64, int) (*domain.MultipartUploadSessionScanPage, error),
) *MultipartUploadSessionCandidateBatchScanner {
	if scanner == nil || listTargets == nil {
		panic("跨对象扫描分片会话需要单对象扫描和分页查询")
	}
	return &MultipartUploadSessionCandidateBatchScanner{scanner: scanner, listTargets: listTargets}
}

// ScanObjectPage 读取一页原对象，并让该页内的对象共用存储页数上限。
// 出错时停止本轮并返回可重试位置。Done 为真表示已读完所有原对象。
func (batch *MultipartUploadSessionCandidateBatchScanner) ScanObjectPage(
	ctx context.Context, start MultipartUploadSessionCandidateCursor, objectLimit, sessionLimit, maxStoragePages int, gracePeriod time.Duration,
) (MultipartUploadSessionCandidateBatch, error) {
	// 1. 校验本轮上限和续扫位置，读取一页原对象。
	result := MultipartUploadSessionCandidateBatch{Next: start}
	if ctx == nil || objectLimit < 1 || sessionLimit < 1 || sessionLimit > domain.MaxMultipartUploadSessionsPageSize ||
		maxStoragePages < 1 || gracePeriod <= 0 {
		return result, fmt.Errorf("跨对象扫描分片会话需要上下文、有效分页上限和正数宽限期")
	}
	afterObjectID := start.ObjectID
	if start.UploadID != "" {
		if start.ObjectID == 0 || !domain.ValidMultipartUploadID(start.UploadID) {
			return result, domain.ErrInvalidUploadTaskData
		}
		// 仓储查询使用 ID > 游标，减一可重新包含正在扫描的对象。
		afterObjectID--
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	page, err := batch.listTargets(ctx, batch.scanner.provider, batch.scanner.bucket, afterObjectID, objectLimit)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !validMultipartUploadSessionScanTargets(page, afterObjectID, objectLimit) {
		return result, domain.ErrInvalidStoredObjectData
	}

	// 2. 按对象 ID 顺序分配剩余页数，汇总会话复核结果。
	for _, target := range page.Targets {
		remaining := maxStoragePages - result.PagesScanned
		if remaining == 0 {
			return result, nil
		}
		marker := ""
		if target.ObjectID == start.ObjectID {
			marker = start.UploadID
		}
		scan, err := batch.scanner.ScanPages(ctx, target, marker, sessionLimit, remaining, gracePeriod)
		result.Objects = append(result.Objects, MultipartUploadSessionObjectScan{Target: target, Scan: scan})
		result.PagesScanned += scan.PagesScanned

		// 3. 记录暂停的位置，或在本页读完后使用数据库游标。
		if scan.NextUploadID != "" {
			result.Next = MultipartUploadSessionCandidateCursor{ObjectID: target.ObjectID, UploadID: scan.NextUploadID}
			return result, err
		}
		if err != nil {
			return result, err
		}
		result.Next = MultipartUploadSessionCandidateCursor{ObjectID: target.ObjectID}
	}

	if page.NextAfterObjectID != 0 {
		result.Next = MultipartUploadSessionCandidateCursor{ObjectID: page.NextAfterObjectID}
	} else {
		result.Next.UploadID = ""
		result.Done = true
	}
	return result, nil
}

func validMultipartUploadSessionScanTargets(page *domain.MultipartUploadSessionScanPage, afterObjectID uint64, limit int) bool {
	if page == nil || len(page.Targets) > limit {
		return false
	}
	previousID := afterObjectID
	for _, target := range page.Targets {
		if target.ObjectID <= previousID {
			return false
		}
		previousID = target.ObjectID
	}
	return page.NextAfterObjectID == 0 || (page.NextAfterObjectID > afterObjectID && page.NextAfterObjectID >= previousID)
}
