package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestExpiredFileVersionBatchScan(t *testing.T) {
	scanFailure := errors.New("测试扫描失败")
	for _, fixture := range []struct {
		name      string
		limit     int
		returned  int
		scanError error
		wantError error
	}{
		{name: "空批次", limit: domain.MaxExpiredFileVersionCandidates},
		{name: "扫描失败", limit: 1, scanError: scanFailure, wantError: scanFailure},
		{name: "扫描结果超限", limit: 1, returned: 2, wantError: domain.ErrInvalidFileVersionData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			scans := 0
			batch := NewExpiredFileVersionBatch(func(ctx context.Context, limit int) ([]domain.FileVersion, error) {
				scans++
				if ctx != t.Context() || limit != fixture.limit {
					t.Fatal("扫描未收到原上下文和批量大小", limit)
				}
				return make([]domain.FileVersion, fixture.returned), fixture.scanError
			}, func(context.Context, domain.FileVersion) error {
				t.Fatal("空批次或扫描失败时不应执行结算")
				return nil
			})
			for _, limit := range []int{0, domain.MaxExpiredFileVersionCandidates + 1} {
				if _, err := batch.RunOnce(t.Context(), limit); err == nil || scans != 0 {
					t.Fatal("无效批量大小进入了扫描", limit, err)
				}
			}
			cancelled, stop := context.WithCancel(t.Context())
			stop()
			if _, err := batch.RunOnce(cancelled, 1); !errors.Is(err, context.Canceled) || scans != 0 {
				t.Fatal("扫描开始前的取消未被保留", err)
			}
			result, err := batch.RunOnce(t.Context(), fixture.limit)
			if !errors.Is(err, fixture.wantError) || scans != 1 || result.Scanned != 0 || result.Settled != 0 ||
				result.StaleCandidates != 0 || len(result.CandidateErrors) != 0 {
				t.Fatal("扫描结果或汇总不符", result, err)
			}
		})
	}
}

func TestExpiredFileVersionBatchPreservesCommittedProgressOnCancellation(t *testing.T) {
	for _, fixture := range []struct {
		name            string
		lastError       error
		expectedSettled int
	}{
		{name: "结算成功后取消", expectedSettled: 2},
		{name: "事务中收到取消", lastError: fmt.Errorf("事务结束: %w", context.Canceled), expectedSettled: 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			candidates := []domain.FileVersion{{ID: 3}, {ID: 1}, {ID: 2}}
			var attempted []uint64
			batch := NewExpiredFileVersionBatch(func(context.Context, int) ([]domain.FileVersion, error) {
				return candidates, nil
			}, func(callContext context.Context, candidate domain.FileVersion) error {
				if callContext != ctx {
					t.Fatal("结算未收到原上下文")
				}
				attempted = append(attempted, candidate.ID)
				if len(attempted) == 2 {
					cancel()
					return fixture.lastError
				}
				return nil
			})
			result, err := batch.RunOnce(ctx, len(candidates))
			if !errors.Is(err, context.Canceled) || !slices.Equal(attempted, []uint64{3, 1}) ||
				result.Scanned != 3 || result.Settled != fixture.expectedSettled || result.StaleCandidates != 0 || len(result.CandidateErrors) != 0 {
				t.Fatal("未保留已提交结果，或取消后仍处理第三项", attempted, result, err)
			}
		})
	}
}
