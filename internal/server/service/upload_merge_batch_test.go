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

func TestUploadMergeBatchScan(t *testing.T) {
	scanError := errors.New("fixture scan error")
	const maximum = domain.MaxMultipartUploadCompletionCandidates
	for _, fixture := range []struct {
		name, owner         string
		limit, count, scans int
		cancel, nilContext  bool
		scanError, want     error
	}{
		{name: "empty at maximum", owner: "worker", limit: maximum, scans: 1},
		{name: "zero limit", owner: "worker", limit: 0},
		{name: "over maximum", owner: "worker", limit: maximum + 1},
		{name: "no context", owner: "worker", limit: 1, nilContext: true},
		{name: "missing worker", limit: 1, want: domain.ErrInvalidUploadTaskData},
		{name: "invalid worker", owner: "检查", limit: 1, want: domain.ErrInvalidUploadTaskData},
		{name: "long worker", owner: strings.Repeat("a", 65), limit: 1, want: domain.ErrInvalidUploadTaskData},
		{name: "oversized scan", owner: "worker", limit: 1, count: 2, scans: 1, want: domain.ErrInvalidUploadTaskData},
		{name: "scan failure", owner: "worker", limit: 1, scans: 1, scanError: scanError, want: scanError},
		{name: "canceled before scan", owner: "worker", limit: 1, cancel: true, want: context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fixture.cancel {
				cancel()
			}
			if fixture.nilContext {
				ctx = nil
			}
			scans := 0
			batch := NewUploadMergeBatch(func(callContext context.Context, limit int) ([]domain.MultipartUploadCompletionCandidate, error) {
				scans++
				if callContext != ctx || limit != fixture.limit {
					t.Fatal("扫描参数不符", limit)
				}
				return make([]domain.MultipartUploadCompletionCandidate, fixture.count), fixture.scanError
			}, func(context.Context, domain.MultipartUploadCompletionCandidate, string) (*domain.UploadTask, error) {
				t.Fatal("没有可用候选时不应领取租约")
				return nil, nil
			}, func(context.Context, *domain.UploadTask) (*domain.UploadTask, error) {
				t.Fatal("没有可用候选时不应执行")
				return nil, nil
			})
			summary, err := batch.RunOnce(ctx, fixture.limit, fixture.owner)
			if fixture.nilContext || fixture.limit < 1 || fixture.limit > maximum {
				if err == nil {
					t.Fatal("未拒绝无效的扫描输入")
				}
			} else if !errors.Is(err, fixture.want) {
				t.Fatal("扫描错误不符", err)
			}
			if scans != fixture.scans || !reflect.DeepEqual(summary, UploadMergeBatchResult{}) {
				t.Fatal("扫描次数或汇总不符", scans, summary)
			}
		})
	}
}

func TestUploadMergeBatchSummarizesInOrder(t *testing.T) {
	const worker = "fixture-worker"
	ctx := t.Context()
	now := time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)
	rollback := errors.New("fixture rollback failure")
	claimError := errors.Join(domain.ErrUploadTaskBusy, rollback)
	executionError := domain.ErrUploadStorageUnavailable
	fixtures := []struct {
		claimErr, executionErr, wantError error
		changeClaim, changeResult         func(*domain.UploadTask)
		status                            domain.UploadStatus
	}{
		{status: domain.UploadStatusVerifying},
		{claimErr: fmt.Errorf("fixture claim: %w", domain.ErrUploadTaskBusy)},
		{status: domain.UploadStatusUploading},
		{claimErr: domain.ErrUploadTaskNotReady},
		{status: domain.UploadStatusFailed},
		{claimErr: domain.ErrUploadTaskNotFound},
		{executionErr: &UploadMergeRetryScheduledError{Cause: executionError, NextAttemptAt: now.Add(time.Minute)}},
		{claimErr: claimError, wantError: rollback},
		{changeClaim: func(task *domain.UploadTask) { task.LeaseVersion++ }, wantError: domain.ErrInvalidUploadTaskData},
		{changeClaim: func(task *domain.UploadTask) { task.LeaseOwner = new("different-worker") }, wantError: domain.ErrInvalidUploadTaskData},
		{executionErr: executionError, wantError: executionError},
		{executionErr: domain.ErrUploadTaskBusy, wantError: domain.ErrUploadTaskBusy},
		{status: domain.UploadStatusVerifying, changeResult: func(task *domain.UploadTask) { task.ID++ }, wantError: domain.ErrInvalidUploadTaskData},
		{status: domain.UploadStatusFailed, changeResult: func(task *domain.UploadTask) { task.LastErrorCode = nil }, wantError: domain.ErrInvalidUploadTaskData},
		{status: domain.UploadStatusCompleting, wantError: domain.ErrInvalidUploadTaskData},
		{status: domain.UploadStatusVerifying},
	}

	// 1. 构造混合候选及期望的逐条领取和执行顺序。
	candidates := make([]domain.MultipartUploadCompletionCandidate, 0, len(fixtures))
	wantCalls := []string{"scan"}
	var wantErrors []UploadMergeCandidateError
	for index, fixture := range fixtures {
		id := uint64(index + 1)
		candidates = append(candidates, domain.MultipartUploadCompletionCandidate{TaskID: id, UserID: 7, StagedObjectID: id + 30, LeaseVersion: 3})
		wantCalls = append(wantCalls, fmt.Sprintf("claim %d", id))
		if fixture.claimErr == nil && fixture.changeClaim == nil {
			wantCalls = append(wantCalls, fmt.Sprintf("execute %d", id))
		}
		if fixture.wantError != nil {
			wantErrors = append(wantErrors, UploadMergeCandidateError{TaskID: id, Err: fixture.wantError})
		}
	}

	// 2. 记录批处理对仓储和单条执行的实际调用。
	var calls []string
	var activeLease *domain.UploadTask
	batch := NewUploadMergeBatch(func(callContext context.Context, limit int) ([]domain.MultipartUploadCompletionCandidate, error) {
		calls = append(calls, "scan")
		if callContext != ctx || limit != len(candidates) {
			t.Fatal("扫描未使用指定上限")
		}
		return candidates, nil
	}, func(callContext context.Context, candidate domain.MultipartUploadCompletionCandidate, owner string) (*domain.UploadTask, error) {
		calls = append(calls, fmt.Sprintf("claim %d", candidate.TaskID))
		if callContext != ctx || owner != worker || candidate != candidates[candidate.TaskID-1] {
			t.Fatal("领取未使用原候选和 worker")
		}
		fixture := fixtures[candidate.TaskID-1]
		if fixture.claimErr != nil {
			return nil, fixture.claimErr
		}
		activeLease = uploadMergeBatchClaimFixture(candidate, owner, now)
		if fixture.changeClaim != nil {
			fixture.changeClaim(activeLease)
		}
		return activeLease, nil
	}, func(callContext context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
		calls = append(calls, fmt.Sprintf("execute %d", claimed.ID))
		if callContext != ctx || claimed != activeLease {
			t.Fatal("执行未使用刚提交的租约")
		}
		fixture := fixtures[claimed.ID-1]
		if fixture.executionErr != nil {
			return nil, fixture.executionErr
		}
		result := copyUploadTask(claimed)
		result.Status, result.LeaseOwner, result.LeaseUntil = fixture.status, nil, nil
		if fixture.status == domain.UploadStatusFailed {
			result.ReservedBytes, result.CompletedAt, result.LastErrorCode = 0, &now, new(domain.UploadSessionLostCode)
		}
		if fixture.changeResult != nil {
			fixture.changeResult(result)
		}
		return result, nil
	})

	// 3. 核对逐条顺序、提交结果与原始错误。
	summary, err := batch.RunOnce(ctx, len(candidates), worker)
	if err != nil || !reflect.DeepEqual(calls, wantCalls) || summary.Scanned != len(fixtures) ||
		summary.Verifying != 2 || summary.Reopened != 1 || summary.Failed != 1 || summary.RetriesScheduled != 1 ||
		summary.Skipped != 3 || len(summary.CandidateErrors) != len(wantErrors) {
		t.Fatal("逐项顺序或结果汇总不符", calls, summary, err)
	}
	for index, want := range wantErrors {
		got := summary.CandidateErrors[index]
		if got.TaskID != want.TaskID || !errors.Is(got.Err, want.Err) {
			t.Fatal("单条错误丢失任务或原因", got, want)
		}
	}
}

func TestUploadMergeBatchPreservesCommittedResultsOnCancellation(t *testing.T) {
	for _, phase := range []string{"after claim", "after verifying", "after retry"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			now := time.Now().UTC()
			candidates := []domain.MultipartUploadCompletionCandidate{{TaskID: 1, UserID: 7, StagedObjectID: 21}, {TaskID: 2, UserID: 7, StagedObjectID: 22}}
			batch := NewUploadMergeBatch(func(context.Context, int) ([]domain.MultipartUploadCompletionCandidate, error) {
				return candidates, nil
			}, func(_ context.Context, candidate domain.MultipartUploadCompletionCandidate, owner string) (*domain.UploadTask, error) {
				if candidate != candidates[0] {
					t.Fatal("取消后仍领取下一项")
				}
				if phase == "after claim" {
					cancel()
				}
				return uploadMergeBatchClaimFixture(candidate, owner, now), nil
			}, func(_ context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
				if phase == "after claim" {
					t.Fatal("领取后取消仍发起执行")
				}
				cancel()
				if phase == "after retry" {
					return nil, &UploadMergeRetryScheduledError{Cause: domain.ErrUploadStorageUnavailable, NextAttemptAt: now.Add(time.Minute)}
				}
				result := copyUploadTask(claimed)
				result.Status, result.LeaseOwner, result.LeaseUntil = domain.UploadStatusVerifying, nil, nil
				return result, nil
			})
			summary, err := batch.RunOnce(ctx, 2, "fixture-worker")
			if !errors.Is(err, context.Canceled) || summary.Scanned != 2 || (summary.Verifying == 1) != (phase == "after verifying") ||
				(summary.RetriesScheduled == 1) != (phase == "after retry") || len(summary.CandidateErrors) != 0 {
				t.Fatal("取消时没有保留已提交结果", summary, err)
			}
		})
	}
}

func uploadMergeBatchClaimFixture(candidate domain.MultipartUploadCompletionCandidate, owner string, now time.Time) *domain.UploadTask {
	task := uploadPartSigningTaskFixture(now.Add(time.Hour))
	task.ID, task.UserID, task.StagedObjectID = candidate.TaskID, candidate.UserID, candidate.StagedObjectID
	task.Status, task.LeaseVersion = domain.UploadStatusCompleting, candidate.LeaseVersion+1
	task.LeaseOwner, task.LeaseUntil = &owner, new(now.Add(time.Minute))
	return task
}
