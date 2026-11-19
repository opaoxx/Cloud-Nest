package service

import (
	"context"
	"errors"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadVerificationBatchScanResults(t *testing.T) {
	scanFailure := errors.New("fixture scan failure")
	for _, scenario := range []struct {
		name          string
		candidates    []domain.UploadVerificationCandidate
		scanError     error
		cancelBefore  bool
		cancelAfter   bool
		wantScanCalls int
		wantError     error
	}{
		{name: "empty scan", wantScanCalls: 1},
		{name: "scan failure", scanError: scanFailure, wantScanCalls: 1, wantError: scanFailure},
		{name: "oversized scan result", candidates: make([]domain.UploadVerificationCandidate, 2), wantScanCalls: 1, wantError: domain.ErrInvalidUploadTaskData},
		{name: "canceled before scan", cancelBefore: true, wantError: context.Canceled},
		{name: "canceled after empty scan", cancelAfter: true, wantScanCalls: 1, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario.cancelBefore {
				cancel()
			}
			scanCalls := 0
			batch := NewUploadVerificationBatch(func(scanContext context.Context, limit int) ([]domain.UploadVerificationCandidate, error) {
				scanCalls++
				if scanContext != ctx || limit != 1 {
					t.Fatal("扫描收到错误的上下文或上限", limit)
				}
				if scenario.cancelAfter {
					cancel()
				}
				return scenario.candidates, scenario.scanError
			}, func(context.Context, domain.UploadVerificationCandidate, string) (*domain.UploadTask, error) {
				t.Fatal("扫描无有效结果时进入了处理流程")
				return nil, nil
			})
			summary, err := batch.RunOnce(ctx, 1, "batch-worker")
			if !errors.Is(err, scenario.wantError) || scanCalls != scenario.wantScanCalls || summary.Scanned != 0 ||
				summary.Published != 0 || summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 0 {
				t.Fatal("扫描结果或错误不符", summary, scanCalls, err)
			}
		})
	}
}

func TestUploadVerificationBatchPreservesResultsOnCancellation(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		completed bool
	}{
		{name: "cancel after publication", completed: true},
		{name: "cancel during candidate"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			first := domain.UploadVerificationCandidate{TaskID: 10, UserID: 20, StagedObjectID: 30}
			second := domain.UploadVerificationCandidate{TaskID: 11, UserID: 20, StagedObjectID: 31}
			processed := 0
			batch := NewUploadVerificationBatch(func(context.Context, int) ([]domain.UploadVerificationCandidate, error) {
				return []domain.UploadVerificationCandidate{first, second}, nil
			}, func(processContext context.Context, candidate domain.UploadVerificationCandidate, owner string) (*domain.UploadTask, error) {
				processed++
				if processContext != ctx || candidate != first || owner != "batch-worker" {
					t.Fatal("处理了错误的候选或未转交调用参数", candidate, owner)
				}
				cancel()
				if scenario.completed {
					return &domain.UploadTask{ID: first.TaskID, UserID: first.UserID, StagedObjectID: first.StagedObjectID, Status: domain.UploadStatusCompleted}, nil
				}
				return nil, processContext.Err()
			})
			summary, err := batch.RunOnce(ctx, 2, "batch-worker")
			if !errors.Is(err, context.Canceled) || processed != 1 || summary.Scanned != 2 || summary.RetriesScheduled != 0 {
				t.Fatal("取消后的扫描汇总错误", summary, processed, err)
			}
			if scenario.completed {
				if summary.Published != 1 || len(summary.CandidateErrors) != 0 {
					t.Fatal("取消丢失了已发布结果", summary)
				}
			} else if summary.Published != 0 || len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != first.TaskID ||
				!errors.Is(summary.CandidateErrors[0].Err, context.Canceled) {
				t.Fatal("取消丢失了当前候选的错误", summary)
			}
		})
	}
}
