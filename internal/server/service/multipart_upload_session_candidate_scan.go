package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadSessionCandidateScan 汇总同一对象已完整复核的会话页。
type MultipartUploadSessionCandidateScan struct {
	MultipartUploadSessionCandidatePage
	PagesScanned int
	// InterruptedFailures 保留中断页已发生的逐项错误，供诊断使用。
	InterruptedFailures []MultipartUploadSessionScanFailure
}

// ScanPages 串行读取同一对象的有限页会话。正常读完时游标为空。
// 读页出错时，游标仍指向该页的读取起点。该页的候选和计数需随重试重新取得。
func (scanner *MultipartUploadSessionCandidateScanner) ScanPages(
	ctx context.Context, target domain.MultipartUploadSessionScanTarget, marker string, limit, maxPages int, gracePeriod time.Duration,
) (MultipartUploadSessionCandidateScan, error) {
	// 1. 校验页数上限，以输入游标作为起点。单页参数由 ScanPage 校验。
	var result MultipartUploadSessionCandidateScan
	result.NextUploadID = marker
	if maxPages < 1 {
		return result, fmt.Errorf("扫描分片会话需要正数页数上限")
	}
	visitedMarkers := map[string]struct{}{marker: {}}

	for result.PagesScanned < maxPages {
		// 2. 串行读取，遇到错误或循环游标就停在当前页。
		page, err := scanner.ScanPage(ctx, target, result.NextUploadID, limit, gracePeriod)
		if err != nil {
			result.InterruptedFailures = page.Failures
			return result, err
		}
		if _, visited := visitedMarkers[page.NextUploadID]; page.NextUploadID != "" && visited {
			result.InterruptedFailures = page.Failures
			return result, fmt.Errorf("分片会话分页游标重复: %w", domain.ErrUploadStorageUnavailable)
		}

		// 3. 汇总完整页，提交下一页游标。空游标表示该对象已读完。
		result.PagesScanned++
		result.Scanned += page.Scanned
		result.Candidates = append(result.Candidates, page.Candidates...)
		result.SkippedBound += page.SkippedBound
		result.SkippedCreation += page.SkippedCreation
		result.SkippedWithinGrace += page.SkippedWithinGrace
		result.Failures = append(result.Failures, page.Failures...)
		result.NextUploadID = page.NextUploadID
		if result.NextUploadID == "" {
			break
		}
		visitedMarkers[result.NextUploadID] = struct{}{}
	}
	return result, nil
}
