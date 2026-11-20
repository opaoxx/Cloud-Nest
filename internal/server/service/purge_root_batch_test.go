package service

import (
	"context"
	"errors"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestPurgeRootBatchScanBoundaries(t *testing.T) {
	scanFailure := errors.New("fixture scan failure")
	for _, scenario := range []struct {
		name                 string
		rootLimit, leafLimit int
		candidates           []domain.PurgeRootCandidate
		scanError, wantError error
		wantScans            int
	}{
		{name: "zero root limit", leafLimit: 1},
		{name: "root limit exceeds maximum", rootLimit: domain.MaxPurgeRootCandidates + 1, leafLimit: 1},
		{name: "zero leaf limit", rootLimit: 1},
		{name: "leaf limit exceeds maximum", rootLimit: 1, leafLimit: domain.MaxPurgeLeafCandidates + 1},
		{name: "scan failure", rootLimit: 1, leafLimit: 1, scanError: scanFailure, wantError: scanFailure, wantScans: 1},
		{name: "too many results", rootLimit: 1, leafLimit: 1, candidates: make([]domain.PurgeRootCandidate, 2),
			wantError: domain.ErrInvalidFileEntryData, wantScans: 1},
		{name: "empty scan", rootLimit: domain.MaxPurgeRootCandidates, leafLimit: domain.MaxPurgeLeafCandidates, wantScans: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 扫描最多调用一次，拒绝无效输入和超限扫描结果。
			scans := 0
			batch := NewPurgeRootBatch(func(ctx context.Context, limit int) ([]domain.PurgeRootCandidate, error) {
				scans++
				if ctx != t.Context() || limit != scenario.rootLimit {
					t.Fatal("扫描没有收到本轮上下文和根项上限", limit)
				}
				return scenario.candidates, scenario.scanError
			}, func(context.Context, domain.PurgeRootCandidate, int) (int, error) {
				t.Fatal("本场景没有可处理的根项")
				return 0, nil
			})
			summary, err := batch.RunOnce(t.Context(), scenario.rootLimit, scenario.leafLimit)
			if scans != scenario.wantScans || summary.Scanned != 0 || summary.CompletedLeaves != 0 ||
				summary.StaleRoots != 0 || len(summary.CandidateErrors) != 0 {
				t.Fatal("扫描边界的汇总不符", scans, summary, err)
			}
			if scenario.wantScans == 0 {
				if err == nil {
					t.Fatal("接受了无效上限")
				}
			} else if !errors.Is(err, scenario.wantError) {
				t.Fatal("扫描错误不符", err)
			}
		})
	}
}

func TestPurgeRootBatchPreservesProgressOnCancellation(t *testing.T) {
	// 1. 第一个根项返回超出上限的计数，第二个成功提交后取消本轮。
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	candidates := []domain.PurgeRootCandidate{
		{UserID: 1, EntryID: 15},
		{UserID: 1, EntryID: 16},
		{UserID: 2, EntryID: 17},
	}
	processed := 0
	batch := NewPurgeRootBatch(func(context.Context, int) ([]domain.PurgeRootCandidate, error) {
		return candidates, nil
	}, func(processContext context.Context, candidate domain.PurgeRootCandidate, limit int) (int, error) {
		if processContext != ctx || limit != 1 || processed >= 2 || candidate != candidates[processed] {
			t.Fatal("结算的上下文、上限或顺序不符", candidate, limit, processed)
		}
		processed++
		if processed == 1 {
			return 2, nil
		}
		cancel()
		return 1, nil
	})

	// 2. 本轮仍返回已提交进度和之前的根项错误，第三项留给后续执行。
	summary, err := batch.RunOnce(ctx, len(candidates), 1)
	if !errors.Is(err, context.Canceled) || processed != 2 || summary.Scanned != 3 || summary.CompletedLeaves != 1 ||
		summary.StaleRoots != 0 || len(summary.CandidateErrors) != 1 {
		t.Fatal("取消时的处理结果不符", processed, summary, err)
	}
	failure := summary.CandidateErrors[0]
	if failure.UserID != 1 || failure.RootID != 15 || !errors.Is(failure.Err, domain.ErrInvalidFileEntryData) {
		t.Fatal("结算计数异常没有归到原根项", failure)
	}
}
