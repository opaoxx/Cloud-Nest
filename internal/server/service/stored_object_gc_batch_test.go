package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestStoredObjectGCBatchScan(t *testing.T) {
	scanFailure := errors.New("扫描数据库失败")
	for _, fixture := range []struct {
		name           string
		limit, scanned int
		scanError      error
		want           error
	}{
		{name: "空批次", limit: domain.MaxStoredObjectGCCandidates},
		{name: "扫描失败", limit: 1, scanError: scanFailure, want: scanFailure},
		{name: "返回行数超过上限", limit: 1, scanned: 2, want: domain.ErrInvalidStoredObjectData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			scanCalls := 0
			batch := NewStoredObjectGCBatch(func(ctx context.Context, limit int) ([]domain.StoredObjectGCCandidate, error) {
				scanCalls++
				if ctx != t.Context() || limit != fixture.limit {
					t.Fatal("未传入原上下文和扫描上限", limit)
				}
				return make([]domain.StoredObjectGCCandidate, fixture.scanned), fixture.scanError
			}, func(context.Context, domain.StoredObjectGCCandidate) (*domain.StoredObject, error) {
				t.Fatal("本用例不应执行任何候选")
				return nil, nil
			})

			for _, limit := range []int{0, domain.MaxStoredObjectGCCandidates + 1} {
				if _, err := batch.RunOnce(t.Context(), limit); err == nil || scanCalls != 0 {
					t.Fatal("无效上限进入扫描", limit, err)
				}
			}
			canceledContext, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := batch.RunOnce(canceledContext, fixture.limit); !errors.Is(err, context.Canceled) || scanCalls != 0 {
				t.Fatal("已取消的上下文进入扫描", err)
			}
			summary, err := batch.RunOnce(t.Context(), fixture.limit)
			if scanCalls != 1 || !errors.Is(err, fixture.want) || summary.Scanned != 0 || summary.Completed != 0 ||
				summary.StaleCandidates != 0 || len(summary.CandidateErrors) != 0 {
				t.Fatal("扫描结果不符", scanCalls, summary, err)
			}
		})
	}
}

func TestStoredObjectGCBatchSummarizesMixedResults(t *testing.T) {
	candidates := []domain.StoredObjectGCCandidate{
		{ObjectID: 33, OwnerUserID: 7},
		{ObjectID: 35, OwnerUserID: 8},
		{ObjectID: 36, OwnerUserID: 8},
		{ObjectID: 37, OwnerUserID: 7},
		{ObjectID: 39, OwnerUserID: 8},
	}
	var processed []domain.StoredObjectGCCandidate
	batch := NewStoredObjectGCBatch(func(context.Context, int) ([]domain.StoredObjectGCCandidate, error) {
		return candidates, nil
	}, func(ctx context.Context, candidate domain.StoredObjectGCCandidate) (*domain.StoredObject, error) {
		if ctx != t.Context() {
			t.Fatal("单项处理未收到原上下文")
		}
		processed = append(processed, candidate)
		completed := &domain.StoredObject{ID: candidate.ObjectID, OwnerUserID: candidate.OwnerUserID, Status: domain.StoredObjectStatusDeleted}
		switch candidate.ObjectID {
		case 33:
			return nil, domain.ErrStoredObjectDeleteUnavailable
		case 35:
			return nil, fmt.Errorf("领取失败: %w", domain.ErrStoredObjectGCCandidateStale)
		case 36:
			return nil, domain.ErrStoredObjectInUse
		case 37:
			completed.OwnerUserID++
		}
		return completed, nil
	})
	summary, err := batch.RunOnce(t.Context(), len(candidates))
	if err != nil || !slices.Equal(processed, candidates) || summary.Scanned != 5 || summary.Completed != 1 ||
		summary.StaleCandidates != 1 || len(summary.CandidateErrors) != 3 {
		t.Fatal("未按扫描顺序处理或汇总不符", processed, summary, err)
	}
	for index, expected := range []struct {
		candidate domain.StoredObjectGCCandidate
		err       error
	}{
		{candidates[0], domain.ErrStoredObjectDeleteUnavailable},
		{candidates[2], domain.ErrStoredObjectInUse},
		{candidates[3], domain.ErrInvalidStoredObjectData},
	} {
		failure := summary.CandidateErrors[index]
		if failure.ObjectID != expected.candidate.ObjectID || failure.OwnerUserID != expected.candidate.OwnerUserID || !errors.Is(failure.Err, expected.err) {
			t.Fatal("逐项失败未保留对象归属或错误原因", failure)
		}
	}
}

func TestStoredObjectGCBatchPreservesCompletedOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	batch := NewStoredObjectGCBatch(func(context.Context, int) ([]domain.StoredObjectGCCandidate, error) {
		return []domain.StoredObjectGCCandidate{{ObjectID: 33, OwnerUserID: 7}, {ObjectID: 35, OwnerUserID: 8}}, nil
	}, func(_ context.Context, candidate domain.StoredObjectGCCandidate) (*domain.StoredObject, error) {
		if candidate.ObjectID != 33 {
			t.Fatal("取消后仍启动下一项", candidate.ObjectID)
		}
		cancel()
		return &domain.StoredObject{ID: candidate.ObjectID, OwnerUserID: candidate.OwnerUserID, Status: domain.StoredObjectStatusDeleted}, nil
	})
	summary, err := batch.RunOnce(ctx, 2)
	if !errors.Is(err, context.Canceled) || summary.Scanned != 2 || summary.Completed != 1 ||
		summary.StaleCandidates != 0 || len(summary.CandidateErrors) != 0 {
		t.Fatal("取消后未保留已提交的进度", summary, err)
	}
}
