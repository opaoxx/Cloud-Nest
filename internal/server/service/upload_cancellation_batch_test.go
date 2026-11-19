package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadCancellationBatchScanBoundaries(t *testing.T) {
	scanFailure := errors.New("fixture scan failed")
	for _, scenario := range []struct {
		name        string
		limit       int
		scanCount   int
		scanError   error
		wantScans   int
		wantError   error
		wantFailure bool
	}{
		{name: "empty scan", limit: 100, wantScans: 1},
		{name: "scanner failure", limit: 2, scanError: scanFailure, wantScans: 1, wantError: scanFailure},
		{name: "scanner exceeds limit", limit: 1, scanCount: 2, wantScans: 1, wantError: domain.ErrInvalidUploadTaskData},
		{name: "zero limit", limit: 0, wantFailure: true},
		{name: "limit above maximum", limit: 101, wantFailure: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 核对扫描只在合法上限内调用，扫描错误与越限结果结束本轮。
			scanCalls := 0
			batch := NewUploadCancellationBatch(func(ctx context.Context, limit int) ([]domain.UploadCancellationCandidate, error) {
				scanCalls++
				if ctx != t.Context() || limit != scenario.limit {
					t.Fatal("扫描未收到本轮上下文或上限", limit)
				}
				return make([]domain.UploadCancellationCandidate, scenario.scanCount), scenario.scanError
			}, func(context.Context, domain.UploadCancellationCandidate) (*domain.UploadTask, error) {
				t.Fatal("空扫描、错误或非法输入仍触发了处理")
				return nil, nil
			})
			summary, err := batch.RunOnce(t.Context(), scenario.limit)
			if scanCalls != scenario.wantScans || summary.Scanned != 0 || summary.Cancelled != 0 || summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 0 {
				t.Fatal("扫描阶段的计数不符", summary, scanCalls, err)
			}
			if scenario.wantFailure {
				if err == nil {
					t.Fatal("非法上限未返回错误")
				}
			} else if !errors.Is(err, scenario.wantError) {
				t.Fatal("扫描阶段的错误不符", err)
			}
		})
	}
}

func TestUploadCancellationBatchChecksScheduledRetries(t *testing.T) {
	candidate := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 3}
	for _, scenario := range []struct {
		name   string
		change func(*domain.UploadTask)
	}{
		{name: "committed retry"},
		{name: "missing retry time", change: func(task *domain.UploadTask) { task.NextAttemptAt = nil }},
		{name: "retry time not after write", change: func(task *domain.UploadTask) { task.NextAttemptAt = &task.UpdatedAt }},
		{name: "no recorded attempt", change: func(task *domain.UploadTask) { task.AttemptCount = 0 }},
		{name: "completion already recorded", change: func(task *domain.UploadTask) { task.CompletedAt = &task.UpdatedAt }},
		{name: "different lease version", change: func(task *domain.UploadTask) { task.LeaseVersion++ }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 让同一候选返回取消中的任务，模拟已提交或不完整的重试安排。
			scheduled := cancelledCandidateResult(candidate)
			scheduled.Status, scheduled.CompletedAt = domain.UploadStatusCancelling, nil
			scheduled.AttemptCount, scheduled.ReservedBytes = 1, 5
			scheduled.UpdatedAt = time.Unix(1, 0).UTC()
			nextAttempt := scheduled.UpdatedAt.Add(time.Second)
			scheduled.NextAttemptAt = &nextAttempt
			if scenario.change != nil {
				scenario.change(scheduled)
			}
			batch := NewUploadCancellationBatch(func(context.Context, int) ([]domain.UploadCancellationCandidate, error) {
				return []domain.UploadCancellationCandidate{candidate}, nil
			}, func(context.Context, domain.UploadCancellationCandidate) (*domain.UploadTask, error) {
				return scheduled, nil
			})

			// 2. 重试成功只计入已安排重试，异常结果计入当前候选错误。
			summary, err := batch.RunOnce(t.Context(), 1)
			if err != nil || summary.Scanned != 1 || summary.Cancelled != 0 {
				t.Fatal("重试结果被误计为取消终态", summary, err)
			}
			if scenario.change == nil {
				if summary.RetriesScheduled != 1 || len(summary.CandidateErrors) != 0 {
					t.Fatal("已提交的重试未正确汇总", summary)
				}
			} else if summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 1 ||
				summary.CandidateErrors[0].TaskID != candidate.TaskID || !errors.Is(summary.CandidateErrors[0].Err, domain.ErrInvalidUploadTaskData) {
				t.Fatal("无效重试结果未计为候选错误", summary)
			}
		})
	}
}

func TestUploadCancellationBatchChecksTerminalResults(t *testing.T) {
	candidate := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 3}
	for _, scenario := range []struct {
		name   string
		change func(*domain.UploadTask)
	}{
		{name: "confirmed cancellation"},
		{name: "another task", change: func(task *domain.UploadTask) { task.ID++ }},
		{name: "changed lease version", change: func(task *domain.UploadTask) { task.LeaseVersion++ }},
		{name: "still cancelling", change: func(task *domain.UploadTask) { task.Status = domain.UploadStatusCancelling }},
		{name: "reserved bytes remain", change: func(task *domain.UploadTask) { task.ReservedBytes = 1 }},
		{name: "no completion time", change: func(task *domain.UploadTask) { task.CompletedAt = nil }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 只把当前候选对应的取消终态计入终结数。
			finished := cancelledCandidateResult(candidate)
			if scenario.change != nil {
				scenario.change(finished)
			}
			batch := NewUploadCancellationBatch(func(context.Context, int) ([]domain.UploadCancellationCandidate, error) {
				return []domain.UploadCancellationCandidate{candidate}, nil
			}, func(context.Context, domain.UploadCancellationCandidate) (*domain.UploadTask, error) {
				return finished, nil
			})
			summary, err := batch.RunOnce(t.Context(), 1)
			if summary.Scanned != 1 || err != nil {
				t.Fatal("批次扫描结果不符", summary, err)
			}
			if scenario.change == nil {
				if summary.Cancelled != 1 || len(summary.CandidateErrors) != 0 {
					t.Fatal("已确认终态未被计入", summary)
				}
			} else if summary.Cancelled != 0 || len(summary.CandidateErrors) != 1 ||
				summary.CandidateErrors[0].TaskID != candidate.TaskID || !errors.Is(summary.CandidateErrors[0].Err, domain.ErrInvalidUploadTaskData) {
				t.Fatal("异常终态未被记录为候选错误", summary)
			}
		})
	}
}

func TestUploadCancellationBatchPreservesResultsOnCancellation(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		committed bool
	}{
		{name: "stop just after commit", committed: true},
		{name: "stop before commit"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 在处理第一项时停止，上层应收到截至此时的结果。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			first := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23}
			second := domain.UploadCancellationCandidate{TaskID: 20, UserID: 7, StagedObjectID: 24}
			batch := NewUploadCancellationBatch(func(context.Context, int) ([]domain.UploadCancellationCandidate, error) {
				return []domain.UploadCancellationCandidate{first, second}, nil
			}, func(processContext context.Context, candidate domain.UploadCancellationCandidate) (*domain.UploadTask, error) {
				if processContext != ctx || candidate != first {
					t.Fatal("停止后仍处理了另一条候选或更换上下文", candidate)
				}
				cancel()
				if scenario.committed {
					return cancelledCandidateResult(candidate), nil
				}
				return nil, processContext.Err()
			})
			summary, err := batch.RunOnce(ctx, 2)
			if !errors.Is(err, context.Canceled) || summary.Scanned != 2 {
				t.Fatal("未返回本轮扫描数和停止原因", summary, err)
			}
			if scenario.committed {
				if summary.Cancelled != 1 || len(summary.CandidateErrors) != 0 {
					t.Fatal("停止时丢失了已提交的终态", summary)
				}
			} else if summary.Cancelled != 0 || len(summary.CandidateErrors) != 1 ||
				summary.CandidateErrors[0].TaskID != first.TaskID || !errors.Is(summary.CandidateErrors[0].Err, context.Canceled) {
				t.Fatal("停止时丢失了当前候选的错误", summary)
			}
		})
	}
}

func cancelledCandidateResult(candidate domain.UploadCancellationCandidate) *domain.UploadTask {
	completedAt := time.Unix(1, 0).UTC()
	return &domain.UploadTask{
		ID: candidate.TaskID, UserID: candidate.UserID, StagedObjectID: candidate.StagedObjectID,
		LeaseVersion: candidate.LeaseVersion, UploadMode: "SINGLE", Status: domain.UploadStatusCancelled, CompletedAt: &completedAt,
	}
}
