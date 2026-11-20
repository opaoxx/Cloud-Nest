package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestExpiredTrashRootBatchScan(t *testing.T) {
	scanFailure := errors.New("测试扫描失败")
	for _, fixture := range []struct {
		name      string
		limit     int
		returned  int
		scanError error
		wantError error
	}{
		{name: "空批次", limit: domain.MaxExpiredTrashRootCandidates},
		{name: "扫描失败", limit: 1, scanError: scanFailure, wantError: scanFailure},
		{name: "结果超过上限", limit: 1, returned: 2, wantError: domain.ErrInvalidFileEntryData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			scans := 0
			batch := NewExpiredTrashRootBatch(func(ctx context.Context, limit int) ([]domain.ExpiredTrashRootCandidate, error) {
				scans++
				if ctx != t.Context() || limit != fixture.limit {
					t.Fatal("扫描未收到原上下文和上限", limit)
				}
				return make([]domain.ExpiredTrashRootCandidate, fixture.returned), fixture.scanError
			}, func(context.Context, domain.ExpiredTrashRootCandidate) (*domain.PurgeRootCandidate, error) {
				t.Fatal("空批次或扫描失败时不应标记")
				return nil, nil
			})
			for _, limit := range []int{0, domain.MaxExpiredTrashRootCandidates + 1} {
				if _, err := batch.RunOnce(t.Context(), limit); err == nil || scans != 0 {
					t.Fatal("无效上限进入了扫描", limit, err)
				}
			}
			result, err := batch.RunOnce(t.Context(), fixture.limit)
			if scans != 1 || !errors.Is(err, fixture.wantError) || result.Scanned != 0 || result.Marked != 0 ||
				result.StaleCandidates != 0 || len(result.CandidateErrors) != 0 {
				t.Fatal("空批次或扫描失败的结果不符", scans, result, err)
			}
		})
	}
}

func TestExpiredTrashRootBatchReportsInvalidResultAndPreservesProgressOnStop(t *testing.T) {
	// 1. 第一项返回异常版本，第二项成功提交后收到取消信号。
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	deletedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	startedAt := deletedAt.AddDate(0, 0, 31)
	candidates := []domain.ExpiredTrashRootCandidate{
		{EntryID: 14, UserID: 1, ParentID: 11, Revision: 2, DeletedAt: deletedAt},
		{EntryID: 22, UserID: 2, ParentID: 21, Revision: 5, DeletedAt: deletedAt},
		{EntryID: 25, UserID: 2, ParentID: 21, Revision: 3, DeletedAt: deletedAt},
	}
	marks := 0
	batch := NewExpiredTrashRootBatch(func(context.Context, int) ([]domain.ExpiredTrashRootCandidate, error) {
		return candidates, nil
	}, func(callContext context.Context, candidate domain.ExpiredTrashRootCandidate) (*domain.PurgeRootCandidate, error) {
		if marks >= 2 || callContext != ctx || candidate != candidates[marks] {
			t.Fatal("标记顺序、上下文或取消后的调用不符", marks, candidate)
		}
		marks++
		root := &domain.PurgeRootCandidate{
			EntryID: candidate.EntryID, UserID: candidate.UserID, ParentID: candidate.ParentID,
			Revision: candidate.Revision, PurgeStartedAt: startedAt,
		}
		if marks == 2 {
			root.Revision++
			cancel()
		}
		return root, nil
	})

	// 2. 保留已提交结果和前一项错误，第三项不再开启事务。
	result, err := batch.RunOnce(ctx, len(candidates))
	if !errors.Is(err, context.Canceled) || marks != 2 || result.Scanned != 3 ||
		result.Marked != 1 || result.StaleCandidates != 0 || len(result.CandidateErrors) != 1 {
		t.Fatal("取消后的本轮结果不符", marks, result, err)
	}
	failure := result.CandidateErrors[0]
	if failure.UserID != candidates[0].UserID || failure.RootID != candidates[0].EntryID || !errors.Is(failure.Err, domain.ErrInvalidFileEntryData) {
		t.Fatal("异常返回未记录候选和错误原因", failure)
	}
}
