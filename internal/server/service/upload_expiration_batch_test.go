package service

import (
	"context"
	"errors"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadExpirationBatchReturnsScanError(t *testing.T) {
	scanFailure := errors.New("fixture scan failed")
	batch := NewUploadExpirationBatch(func(ctx context.Context, limit int) ([]domain.UploadExpirationCandidate, error) {
		if ctx != t.Context() || limit != 10 {
			t.Fatal("扫描没有收到本轮的上下文和上限", limit)
		}
		return nil, scanFailure
	}, func(context.Context, domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
		t.Fatal("扫描失败后仍调用了候选处理")
		return nil, nil
	})
	summary, err := batch.RunOnce(t.Context(), 10)
	if !errors.Is(err, scanFailure) || summary.Scanned != 0 || summary.Expired != 0 || len(summary.CandidateErrors) != 0 {
		t.Fatal("扫描失败的汇总或错误不符", summary, err)
	}
}

func TestUploadExpirationBatchPreservesResultsOnCancellation(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		completed bool
	}{
		{name: "cancel after expiry committed", completed: true},
		{name: "cancel before candidate finished"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			first := domain.UploadExpirationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, UploadMode: "SINGLE"}
			second := domain.UploadExpirationCandidate{TaskID: 20, UserID: 7, StagedObjectID: 24, UploadMode: "SINGLE"}
			processed := 0
			batch := NewUploadExpirationBatch(func(context.Context, int) ([]domain.UploadExpirationCandidate, error) {
				return []domain.UploadExpirationCandidate{first, second}, nil
			}, func(processContext context.Context, candidate domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
				processed++
				if processContext != ctx || candidate != first {
					t.Fatal("取消后处理了另一条候选或换用了上下文", candidate)
				}
				cancel()
				if scenario.completed {
					return &domain.UploadTask{ID: first.TaskID, UserID: first.UserID, StagedObjectID: first.StagedObjectID,
						Status: domain.UploadStatusExpired, UploadMode: first.UploadMode}, nil
				}
				return nil, processContext.Err()
			})
			summary, err := batch.RunOnce(ctx, 2)
			if !errors.Is(err, context.Canceled) || processed != 1 || summary.Scanned != 2 {
				t.Fatal("取消后的批次结果不符", summary, processed, err)
			}
			if scenario.completed {
				if summary.Expired != 1 || len(summary.CandidateErrors) != 0 {
					t.Fatal("取消丢失了已提交结果", summary)
				}
			} else if summary.Expired != 0 || len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != first.TaskID ||
				!errors.Is(summary.CandidateErrors[0].Err, context.Canceled) {
				t.Fatal("取消时丢失了当前候选错误", summary)
			}
		})
	}
}
