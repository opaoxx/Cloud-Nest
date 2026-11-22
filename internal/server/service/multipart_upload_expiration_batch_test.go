package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadExpirationBatchScan(t *testing.T) {
	scanFailure := errors.New("fixture scan failed")
	for _, fixture := range []struct {
		name, owner        string
		limit, returned    int
		canceled, wantScan bool
		scanError, want    error
		invalid            bool
	}{
		{name: "empty at upper limit", owner: "fixture", limit: domain.MaxUploadExpirationCandidates, wantScan: true},
		{name: "zero limit", owner: "fixture", invalid: true},
		{name: "above limit", owner: "fixture", limit: domain.MaxUploadExpirationCandidates + 1, invalid: true},
		{name: "invalid owner", owner: "执行者", limit: 1, want: domain.ErrInvalidUploadTaskData},
		{name: "already canceled", owner: "fixture", limit: 1, canceled: true, want: context.Canceled},
		{name: "scan failed", owner: "fixture", limit: 1, wantScan: true, scanError: scanFailure, want: scanFailure},
		{name: "oversized result", owner: "fixture", limit: 1, returned: 2, wantScan: true, want: domain.ErrInvalidUploadTaskData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fixture.canceled {
				cancel()
			}
			scans := 0
			batch := NewMultipartUploadExpirationBatch(
				func(callContext context.Context, limit int) ([]domain.UploadExpirationCandidate, error) {
					scans++
					if callContext != ctx || limit != fixture.limit {
						t.Fatal("扫描未收到原上下文和上限")
					}
					return make([]domain.UploadExpirationCandidate, fixture.returned), fixture.scanError
				},
				func(context.Context, domain.UploadExpirationCandidate, string) (*domain.UploadTask, error) {
					t.Fatal("没有有效候选时进行了领取")
					return nil, nil
				},
				func(context.Context, *domain.UploadTask) (*domain.UploadTask, error) {
					t.Fatal("没有有效候选时进行了执行")
					return nil, nil
				})
			summary, err := batch.RunOnce(ctx, fixture.limit, fixture.owner)
			if (!fixture.invalid && !errors.Is(err, fixture.want)) || (fixture.invalid && err == nil) ||
				(scans == 1) != fixture.wantScan || scans > 1 || !reflect.DeepEqual(summary, MultipartUploadExpirationBatchResult{}) {
				t.Fatal("扫描边界或返回结果不符", scans, summary, err)
			}
		})
	}
}

func TestMultipartUploadExpirationBatchReportsInOrder(t *testing.T) {
	const owner = "fixture-expiry"
	ctx := t.Context()
	now := time.Now().UTC()
	rollback := errors.New("fixture rollback failure")
	invalid := domain.ErrInvalidUploadTaskData
	storageCause := domain.ErrUploadStorageUnavailable
	retry := &MultipartUploadExpirationRetryScheduledError{Cause: storageCause, NextAttemptAt: now.Add(time.Minute)}
	for _, fixture := range []struct {
		name                      string
		claimError, executeError  error
		editsClaim, editsResult   func(*domain.UploadTask)
		singleCandidate           bool
		expired, retries, skipped int
		want                      error
	}{
		{name: "expired", expired: 1},
		{name: "registered retry at lease boundary", executeError: fmt.Errorf("fixture: %w", retry), retries: 1},
		{name: "early retry", executeError: &MultipartUploadExpirationRetryScheduledError{Cause: storageCause, NextAttemptAt: now}, want: invalid},
		{name: "retry with independent failure", executeError: errors.Join(retry, rollback), want: rollback},
		{name: "stale during claim", claimError: fmt.Errorf("fixture: %w", domain.ErrUploadTaskBusy), skipped: 1},
		{name: "stale during execution", executeError: domain.ErrUploadTaskNotReady, skipped: 1},
		{name: "stale and rollback failure", executeError: errors.Join(domain.ErrUploadTaskBusy, rollback), want: rollback},
		{name: "independent storage failure", executeError: storageCause, want: storageCause},
		{name: "single mode candidate", singleCandidate: true, want: invalid},
		{name: "wrong claimed expiry", editsClaim: func(task *domain.UploadTask) { task.ExpiresAt = task.ExpiresAt.Add(time.Second) }, want: invalid},
		{name: "wrong claimed lease version", editsClaim: func(task *domain.UploadTask) { task.LeaseVersion++ }, want: invalid},
		{name: "wrong claimed owner", editsClaim: func(task *domain.UploadTask) { task.LeaseOwner = new("different") }, want: invalid},
		{name: "wrong terminal", editsResult: func(task *domain.UploadTask) { task.Status = domain.UploadStatusCancelled }, want: invalid},
		{name: "unsettled reservation", editsResult: func(task *domain.UploadTask) { task.ReservedBytes = 1 }, want: invalid},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			first, second := multipartExpirationBatchCandidates(now)
			if fixture.singleCandidate {
				first.UploadMode = "SINGLE"
			}
			var order []string
			var active *domain.UploadTask
			batch := NewMultipartUploadExpirationBatch(
				func(_ context.Context, limit int) ([]domain.UploadExpirationCandidate, error) {
					order = append(order, "scan")
					if limit != 2 {
						t.Fatal("扫描上限不符", limit)
					}
					return []domain.UploadExpirationCandidate{first, second}, nil
				},
				func(callContext context.Context, candidate domain.UploadExpirationCandidate, leaseOwner string) (*domain.UploadTask, error) {
					order = append(order, fmt.Sprintf("claim %d", candidate.TaskID))
					if callContext != ctx || leaseOwner != owner || (candidate != first && candidate != second) {
						t.Fatal("领取未使用原候选或执行者")
					}
					if candidate == first && fixture.claimError != nil {
						return nil, fixture.claimError
					}
					active = multipartExpirationBatchClaim(candidate, leaseOwner, now)
					if candidate == first && fixture.editsClaim != nil {
						fixture.editsClaim(active)
					}
					return active, nil
				},
				func(callContext context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
					order = append(order, fmt.Sprintf("execute %d", claimed.ID))
					if callContext != ctx || claimed != active {
						t.Fatal("执行未使用刚领取的租约")
					}
					if claimed.ID == first.TaskID && fixture.executeError != nil {
						return nil, fixture.executeError
					}
					finished := multipartExpirationBatchFinished(claimed)
					if claimed.ID == first.TaskID && fixture.editsResult != nil {
						fixture.editsResult(finished)
					}
					return finished, nil
				})
			wantOrder := []string{"scan"}
			if !fixture.singleCandidate {
				wantOrder = append(wantOrder, "claim 19")
				if fixture.claimError == nil && fixture.editsClaim == nil {
					wantOrder = append(wantOrder, "execute 19")
				}
			}
			wantOrder = append(wantOrder, "claim 20", "execute 20")
			summary, err := batch.RunOnce(ctx, 2, owner)
			if err != nil || !reflect.DeepEqual(order, wantOrder) || summary.Scanned != 2 || summary.Expired != fixture.expired+1 ||
				summary.RetriesScheduled != fixture.retries || summary.Skipped != fixture.skipped {
				t.Fatal("单轮顺序或汇总不符", order, summary, err)
			}
			if fixture.want == nil && len(summary.CandidateErrors) != 0 {
				t.Fatal("已经处理的候选被计为故障", summary.CandidateErrors)
			}
			if fixture.want != nil && (len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != first.TaskID ||
				!errors.Is(summary.CandidateErrors[0].Err, fixture.want)) {
				t.Fatal("未保留逐项故障", summary.CandidateErrors)
			}
		})
	}
}

func TestMultipartUploadExpirationBatchPreservesResultsOnCancellation(t *testing.T) {
	for _, phase := range []string{"scan", "claim", "completed", "retry"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			now := time.Now().UTC()
			first, second := multipartExpirationBatchCandidates(now)
			batch := NewMultipartUploadExpirationBatch(
				func(context.Context, int) ([]domain.UploadExpirationCandidate, error) {
					if phase == "scan" {
						cancel()
					}
					return []domain.UploadExpirationCandidate{first, second}, nil
				},
				func(_ context.Context, candidate domain.UploadExpirationCandidate, owner string) (*domain.UploadTask, error) {
					if candidate != first || phase == "scan" {
						t.Fatal("停止后仍在领取候选")
					}
					if phase == "claim" {
						cancel()
					}
					return multipartExpirationBatchClaim(candidate, owner, now), nil
				},
				func(_ context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
					if phase == "claim" {
						t.Fatal("停止后仍在执行候选")
					}
					cancel()
					if phase == "retry" {
						return nil, &MultipartUploadExpirationRetryScheduledError{Cause: domain.ErrUploadStorageUnavailable, NextAttemptAt: *claimed.LeaseUntil}
					}
					return multipartExpirationBatchFinished(claimed), nil
				})
			summary, err := batch.RunOnce(ctx, 2, "fixture-expiry")
			if !errors.Is(err, context.Canceled) || summary.Scanned != 2 || (summary.Expired == 1) != (phase == "completed") ||
				(summary.RetriesScheduled == 1) != (phase == "retry") || summary.Skipped != 0 || len(summary.CandidateErrors) != 0 {
				t.Fatal("取消时的部分结果不符", summary, err)
			}
		})
	}
}

func multipartExpirationBatchCandidates(now time.Time) (domain.UploadExpirationCandidate, domain.UploadExpirationCandidate) {
	return domain.UploadExpirationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 4,
			Status: domain.UploadStatusUploading, UploadMode: "MULTIPART", ExpiresAt: now.Add(-time.Minute)},
		domain.UploadExpirationCandidate{TaskID: 20, UserID: 8, StagedObjectID: 24, LeaseVersion: 5,
			Status: domain.UploadStatusExpiring, UploadMode: "MULTIPART", ExpiresAt: now.Add(-time.Second)}
}

func multipartExpirationBatchClaim(candidate domain.UploadExpirationCandidate, owner string, now time.Time) *domain.UploadTask {
	return &domain.UploadTask{ID: candidate.TaskID, UserID: candidate.UserID, StagedObjectID: candidate.StagedObjectID,
		Status: domain.UploadStatusExpiring, UploadMode: "MULTIPART", ExpiresAt: candidate.ExpiresAt,
		FileSize: 16777217, ReservedBytes: 16777217, UpdatedAt: now,
		LeaseOwner: new(owner), LeaseUntil: new(now.Add(time.Minute)), LeaseVersion: candidate.LeaseVersion + 1}
}

func multipartExpirationBatchFinished(claimed *domain.UploadTask) *domain.UploadTask {
	finished := copyUploadTask(claimed)
	finished.Status, finished.ReservedBytes = domain.UploadStatusExpired, 0
	finished.LeaseOwner, finished.LeaseUntil = nil, nil
	finished.UpdatedAt = finished.UpdatedAt.Add(time.Second)
	finished.CompletedAt = new(finished.UpdatedAt)
	return finished
}
