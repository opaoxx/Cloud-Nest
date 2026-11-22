package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func multipartCancellationBatchClaim(candidate domain.UploadCancellationCandidate, owner string, now time.Time) *domain.UploadTask {
	const size = domain.MaxSingleUploadSizeBytes + 1
	return &domain.UploadTask{ID: candidate.TaskID, UserID: candidate.UserID, StagedObjectID: candidate.StagedObjectID,
		UploadMode: "MULTIPART", Status: domain.UploadStatusCancelling, FileSize: size, ReservedBytes: size,
		LeaseOwner: new(owner), LeaseUntil: new(now.Add(time.Minute)), LeaseVersion: candidate.LeaseVersion + 1,
		UpdatedAt: now, ExpiresAt: now.Add(-time.Minute)}
}

func multipartCancellationBatchFinished(claimed *domain.UploadTask) *domain.UploadTask {
	task := copyUploadTask(claimed)
	task.Status, task.ReservedBytes = domain.UploadStatusCancelled, 0
	task.LeaseOwner, task.LeaseUntil = nil, nil
	task.UpdatedAt = task.UpdatedAt.Add(time.Second)
	task.CompletedAt = new(task.UpdatedAt)
	return task
}

func TestMultipartUploadCancellationBatchScan(t *testing.T) {
	scanError := errors.New("fixture scan error")
	for _, fixture := range []struct {
		name, owner                    string
		limit, scanCount               int
		nilContext, canceled, wantScan bool
		wantError                      bool
		scanError, cause               error
	}{
		{name: "empty batch at upper bound", owner: strings.Repeat("a", 64), limit: 100, wantScan: true},
		{name: "limit zero", owner: "fixture", wantError: true},
		{name: "limit exceeds one hundred", owner: "fixture", limit: 101, wantError: true},
		{name: "empty owner", limit: 1, wantError: true, cause: domain.ErrInvalidUploadTaskData},
		{name: "too long owner", owner: strings.Repeat("a", 65), limit: 1, wantError: true, cause: domain.ErrInvalidUploadTaskData},
		{name: "non-ASCII owner", owner: "执行者", limit: 1, wantError: true, cause: domain.ErrInvalidUploadTaskData},
		{name: "nil context", nilContext: true, owner: "fixture", limit: 1, wantError: true},
		{name: "canceled before scan", canceled: true, owner: "fixture", limit: 1, wantError: true, cause: context.Canceled},
		{name: "scan failed", owner: "fixture", limit: 1, scanError: scanError, wantScan: true, wantError: true, cause: scanError},
		{name: "scan exceeded requested limit", owner: "fixture", limit: 1, scanCount: 2, wantScan: true, wantError: true, cause: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fixture.canceled {
				cancel()
			}
			var batchContext context.Context = ctx
			if fixture.nilContext {
				batchContext = nil
			}
			scans := 0
			batch := NewMultipartUploadCancellationBatch(
				func(callContext context.Context, limit int) ([]domain.UploadCancellationCandidate, error) {
					scans++
					if callContext != ctx || limit != fixture.limit {
						t.Fatal("未使用指定上下文和批量上限")
					}
					return make([]domain.UploadCancellationCandidate, fixture.scanCount), fixture.scanError
				},
				func(context.Context, domain.UploadCancellationCandidate, string) (*domain.UploadTask, error) {
					t.Fatal("无有效扫描时领到了租约")
					return nil, nil
				},
				func(context.Context, *domain.UploadTask) (*domain.UploadTask, error) {
					t.Fatal("无有效扫描时执行了任务")
					return nil, nil
				})
			summary, err := batch.RunOnce(batchContext, fixture.limit, fixture.owner)
			if (err != nil) != fixture.wantError || (fixture.cause != nil && !errors.Is(err, fixture.cause)) ||
				scans > 1 || (scans == 1) != fixture.wantScan || !reflect.DeepEqual(summary, MultipartUploadCancellationBatchResult{}) {
				t.Fatal("扫描边界或调用次数不符", err, scans, summary)
			}
		})
	}
}

func TestMultipartUploadCancellationBatchSummarizesInOrder(t *testing.T) {
	const worker = "fixture-cancel-worker"
	ctx := t.Context()
	now := time.Now().UTC()
	storageCause := domain.ErrUploadStorageUnavailable
	busy := domain.ErrUploadTaskBusy
	invalid := domain.ErrInvalidUploadTaskData
	rollback := errors.New("fixture rollback error")
	retry := &MultipartUploadCancellationRetryScheduledError{Cause: storageCause, NextAttemptAt: now.Add(time.Minute)}
	for _, fixture := range []struct {
		name                                     string
		claimError, executeError                 error
		changeClaim, changeResult                func(*domain.UploadTask)
		changeCandidate                          func(*domain.UploadCancellationCandidate)
		want                                     error
		countCancelled, countRetry, countSkipped int
	}{
		{name: "ordinary claim failure", claimError: storageCause, want: storageCause},
		{name: "stale claim", claimError: fmt.Errorf("fixture: %w", busy), countSkipped: 1},
		{name: "claim error with rollback failure", claimError: errors.Join(busy, rollback), want: rollback},
		{name: "changed claimed object", changeClaim: func(task *domain.UploadTask) { task.StagedObjectID++ }, want: invalid},
		{name: "changed claimed lease version", changeClaim: func(task *domain.UploadTask) { task.LeaseVersion++ }, want: invalid},
		{name: "wrong worker", changeClaim: func(task *domain.UploadTask) { task.LeaseOwner = new("other-worker") }, want: invalid},
		{name: "invalid candidate", changeCandidate: func(candidate *domain.UploadCancellationCandidate) { candidate.UserID = 0 }, want: invalid},
		{name: "valid retry", executeError: fmt.Errorf("fixture: %w", retry), countRetry: 1},
		{name: "invalid early retry", executeError: &MultipartUploadCancellationRetryScheduledError{Cause: storageCause, NextAttemptAt: now}, want: invalid},
		{name: "retry signal with independent error", executeError: errors.Join(retry, rollback), want: rollback},
		{name: "stale after execution", executeError: fmt.Errorf("fixture: %w", domain.ErrUploadTaskNotReady), countSkipped: 1},
		{name: "execute and rollback error", executeError: errors.Join(busy, rollback), want: rollback},
		{name: "wrong terminal binding", changeResult: func(task *domain.UploadTask) { task.ID++ }, want: invalid},
		{name: "terminal reservation not settled", changeResult: func(task *domain.UploadTask) { task.ReservedBytes = 1 }, want: invalid},
		{name: "valid terminal", countCancelled: 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			first := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 4}
			last := domain.UploadCancellationCandidate{TaskID: 20, UserID: 8, StagedObjectID: 24, LeaseVersion: 5}
			if fixture.changeCandidate != nil {
				fixture.changeCandidate(&first)
			}
			var calls []string
			var active *domain.UploadTask
			batch := NewMultipartUploadCancellationBatch(
				func(callContext context.Context, limit int) ([]domain.UploadCancellationCandidate, error) {
					calls = append(calls, "scan")
					if callContext != ctx || limit != 2 {
						t.Fatal("未按上限扫描")
					}
					return []domain.UploadCancellationCandidate{first, last}, nil
				},
				func(callContext context.Context, candidate domain.UploadCancellationCandidate, owner string) (*domain.UploadTask, error) {
					calls = append(calls, fmt.Sprintf("claim %d", candidate.TaskID))
					if callContext != ctx || owner != worker || (candidate != first && candidate != last) {
						t.Fatal("未使用原候选领取租约")
					}
					if candidate == first && fixture.claimError != nil {
						return nil, fixture.claimError
					}
					active = multipartCancellationBatchClaim(candidate, worker, now)
					if candidate == first && fixture.changeClaim != nil {
						fixture.changeClaim(active)
					}
					return active, nil
				},
				func(callContext context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
					calls = append(calls, fmt.Sprintf("execute %d", claimed.ID))
					if claimed != active || callContext != ctx {
						t.Fatal("未使用本轮原租约")
					}
					if claimed.ID == first.TaskID && fixture.executeError != nil {
						return nil, fixture.executeError
					}
					finished := multipartCancellationBatchFinished(claimed)
					if claimed.ID == first.TaskID && fixture.changeResult != nil {
						fixture.changeResult(finished)
					}
					return finished, nil
				})
			wantCalls := []string{"scan"}
			if fixture.changeCandidate == nil {
				wantCalls = append(wantCalls, "claim 19")
				if fixture.claimError == nil && fixture.changeClaim == nil {
					wantCalls = append(wantCalls, "execute 19")
				}
			}
			wantCalls = append(wantCalls, "claim 20", "execute 20")
			summary, err := batch.RunOnce(ctx, 2, worker)
			if err != nil || !reflect.DeepEqual(calls, wantCalls) || summary.Scanned != 2 || summary.Cancelled != fixture.countCancelled+1 ||
				summary.RetriesScheduled != fixture.countRetry || summary.Skipped != fixture.countSkipped {
				t.Fatal("顺序或汇总计数不符", calls, summary, err)
			}
			if fixture.want == nil {
				if len(summary.CandidateErrors) != 0 {
					t.Fatal("有效结果被计为故障", summary.CandidateErrors)
				}
			} else if len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != first.TaskID ||
				!errors.Is(summary.CandidateErrors[0].Err, fixture.want) {
				t.Fatal("逐项错误没有保留原任务或原因", summary.CandidateErrors)
			}
		})
	}
}

func TestMultipartUploadCancellationBatchPreservesResultsOnCancellation(t *testing.T) {
	failure := errors.New("fixture failure during shutdown")
	for _, phase := range []string{"scan", "claim", "plain error", "combined error", "committed cancellation", "committed retry"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			now := time.Now().UTC()
			first := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 4}
			second := domain.UploadCancellationCandidate{TaskID: 20, UserID: 8, StagedObjectID: 24, LeaseVersion: 4}
			batch := NewMultipartUploadCancellationBatch(
				func(context.Context, int) ([]domain.UploadCancellationCandidate, error) {
					if phase == "scan" {
						cancel()
					}
					return []domain.UploadCancellationCandidate{first, second}, nil
				},
				func(_ context.Context, candidate domain.UploadCancellationCandidate, owner string) (*domain.UploadTask, error) {
					if candidate != first || phase == "scan" {
						t.Fatal("取消后仍领取任务")
					}
					if phase == "claim" {
						cancel()
					}
					return multipartCancellationBatchClaim(candidate, owner, now), nil
				},
				func(_ context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
					if phase == "claim" {
						t.Fatal("取消后仍执行任务")
					}
					cancel()
					switch phase {
					case "plain error":
						return nil, fmt.Errorf("fixture: %w", context.Canceled)
					case "combined error":
						return nil, errors.Join(context.Canceled, failure)
					case "committed retry":
						return nil, &MultipartUploadCancellationRetryScheduledError{Cause: domain.ErrUploadStorageUnavailable, NextAttemptAt: *claimed.LeaseUntil}
					default:
						return multipartCancellationBatchFinished(claimed), nil
					}
				})
			summary, err := batch.RunOnce(ctx, 2, "fixture-worker")
			if !errors.Is(err, context.Canceled) || summary.Scanned != 2 ||
				(summary.Cancelled == 1) != (phase == "committed cancellation") ||
				(summary.RetriesScheduled == 1) != (phase == "committed retry") ||
				(len(summary.CandidateErrors) == 1) != (phase == "combined error") {
				t.Fatal("停止时丢失已提交结果或错误", summary, err)
			}
			if phase == "combined error" && !errors.Is(summary.CandidateErrors[0].Err, failure) {
				t.Fatal("停止时丢失了独立故障")
			}
		})
	}
}
