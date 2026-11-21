package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadMergeExecutionServiceExecute(t *testing.T) {
	const provider, bucket, key, session = "local_minio", "fixture-bucket", "users/7/objects/merge +%.bin", "Opaque+A/= "
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	full := []string{"preflight", "complete", "write"}
	completionRetry := []string{"preflight", "complete", "retry"}
	writeRetry := []string{"preflight", "complete", "write", "retry"}
	failure := errors.New("fixture write failed")
	retryableWrite := errors.Join(domain.ErrUploadMergeWriteRetryable, failure)
	for _, fixture := range []struct {
		name, mode, cancelAt                string
		preflightErr, completeErr, writeErr error
		retryErr                            error
		attemptCount                        uint32
		wantDelay                           int
		want                                error
		wantCalls                           []string
	}{
		{name: "complete and write", wantCalls: full},
		{name: "recovered version writes directly", mode: "recovered", wantCalls: []string{"preflight", "write"}},
		{name: "preflight rejects stale lease", preflightErr: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: full[:1]},
		{name: "no action supplied", mode: "none", want: domain.ErrInvalidUploadTaskData, wantCalls: full[:1]},
		{name: "contradictory actions", mode: "both", want: domain.ErrInvalidUploadTaskData, wantCalls: full[:1]},
		{name: "target belongs to another session", mode: "wrong session", want: domain.ErrInvalidUploadTaskData, wantCalls: full[:1]},
		{name: "receipt list incomplete", mode: "short receipts", want: domain.ErrInvalidUploadTaskData, wantCalls: full[:1]},
		{name: "unknown completion schedules retry", completeErr: domain.ErrMultipartUploadCompletionUnknown,
			want: domain.ErrMultipartUploadCompletionUnknown, wantCalls: completionRetry, wantDelay: 1},
		{name: "temporary storage error backs off", preflightErr: domain.ErrUploadStorageUnavailable,
			want: domain.ErrUploadStorageUnavailable, wantCalls: []string{"preflight", "retry"}, attemptCount: 4, wantDelay: 16},
		{name: "missing session needs recheck", preflightErr: domain.ErrUploadPartSessionMissing,
			want: domain.ErrUploadPartSessionMissing, wantCalls: []string{"preflight", "retry"}, wantDelay: 1},
		{name: "storage deadline caps backoff", completeErr: context.DeadlineExceeded,
			want: context.DeadlineExceeded, wantCalls: completionRetry, attemptCount: 10, wantDelay: 30},
		{name: "known lock failure schedules retry", writeErr: retryableWrite, want: failure, wantCalls: writeRetry, wantDelay: 1},
		{name: "retry write failure retains both errors", completeErr: domain.ErrMultipartUploadCompletionUnknown, retryErr: domain.ErrUploadTaskBusy,
			want: domain.ErrMultipartUploadCompletionUnknown, wantCalls: completionRetry, wantDelay: 1},
		{name: "invalid retry result retains original error", mode: "wrong retry", completeErr: domain.ErrMultipartUploadCompletionUnknown,
			want: domain.ErrMultipartUploadCompletionUnknown, wantCalls: completionRetry, wantDelay: 1},
		{name: "mismatched parts need separate handling", preflightErr: domain.ErrUploadPartsMismatch,
			want: domain.ErrUploadPartsMismatch, wantCalls: full[:1]},
		{name: "explicit rejection does not write", completeErr: domain.ErrMultipartUploadPartRejected,
			want: domain.ErrMultipartUploadPartRejected, wantCalls: full[:2]},
		{name: "size differs on confirmation", mode: "wrong size", want: domain.ErrUploadBodySizeMismatch, wantCalls: full[:2]},
		{name: "absent confirmation", mode: "nil confirmation", want: domain.ErrUploadStorageUnavailable, wantCalls: full[:2]},
		{name: "write failure does not repeat completion", writeErr: failure, want: failure, wantCalls: full},
		{name: "commit rejected by current lease", writeErr: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: full},
		{name: "wrong committed task", mode: "wrong commit", want: domain.ErrInvalidUploadTaskData, wantCalls: full},
		{name: "canceled after preflight", cancelAt: "preflight", want: context.Canceled, wantCalls: full[:1]},
		{name: "canceled after completion", cancelAt: "complete", want: context.Canceled, wantCalls: full[:2]},
		{name: "canceled with unknown result", cancelAt: "complete", completeErr: errors.Join(domain.ErrMultipartUploadCompletionUnknown, context.Canceled),
			want: domain.ErrMultipartUploadCompletionUnknown, wantCalls: full[:2]},
		{name: "committed success survives caller cancellation", cancelAt: "write", wantCalls: full},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 准备已领取任务、预检证据和独立的预期写回结果。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.Status, claimed.LeaseOwner, claimed.LeaseUntil = domain.UploadStatusCompleting, new("merge-worker"), new(now.Add(time.Minute))
			claimed.StorageUploadID = new(session)
			claimed.AttemptCount = fixture.attemptCount
			original := copyUploadTask(claimed)
			committed := copyUploadTask(claimed)
			committed.Status, committed.LeaseOwner, committed.LeaseUntil = domain.UploadStatusVerifying, nil, nil
			scheduled := copyUploadTask(claimed)
			scheduled.LeaseOwner, scheduled.LeaseUntil = nil, nil
			scheduled.AttemptCount++
			scheduled.UpdatedAt, scheduled.NextAttemptAt = now, new(now.Add(time.Minute))
			target := UploadPartSigningTarget{TaskID: claimed.ID, UserID: claimed.UserID, StagedObjectID: claimed.StagedObjectID,
				LeaseVersion: claimed.LeaseVersion, ObjectKey: key, UploadID: session, FileSizeBytes: claimed.FileSize,
				PartSizeBytes: *claimed.PartSize, PartCount: *claimed.PartCount, ExpiresAt: claimed.ExpiresAt}
			parts := []domain.UploadPartReceipt{uploadPartReceiptFixture(1, *claimed.PartSize, "First"), uploadPartReceiptFixture(2, 1, "Tail")}
			metadata := &domain.UploadObjectResult{SizeBytes: claimed.FileSize, StorageETag: new("object-etag"), StorageVersionID: new("Version+A/=")}
			plan := &UploadMergePreflightResult{Ready: &UploadMergeReceiptSnapshot{Target: target, Parts: parts}}
			switch fixture.mode {
			case "recovered":
				plan = &UploadMergePreflightResult{Recovered: &UploadMergeRecoveryObservation{Target: target, Object: *metadata}}
			case "none":
				plan.Ready = nil
			case "both":
				plan.Recovered = &UploadMergeRecoveryObservation{Target: target, Object: *metadata}
			case "wrong session":
				plan.Ready.Target.UploadID = "different"
			case "short receipts":
				plan.Ready.Parts = parts[:1]
			case "wrong size":
				metadata.SizeBytes--
			case "nil confirmation":
				metadata = nil
			case "wrong commit":
				committed.ID++
			case "wrong retry":
				scheduled.ID++
			}

			// 2. 记录预检、合并和写回，逐项检查传入的原租约、位置及确认。
			var calls []string
			execution := NewUploadMergeExecutionService(provider, bucket,
				func(callContext context.Context, lease *domain.UploadTask) (*UploadMergePreflightResult, error) {
					calls = append(calls, "preflight")
					if callContext != ctx || lease != claimed {
						t.Fatal("预检参数不符")
					}
					if fixture.cancelAt == "preflight" {
						cancel()
					}
					return plan, fixture.preflightErr
				},
				func(callContext context.Context, objectKey, uploadID string, size, partSize uint64, partCount uint32, supplied []domain.UploadPartReceipt) (*domain.UploadObjectResult, error) {
					calls = append(calls, "complete")
					if callContext != ctx || objectKey != key || uploadID != session || size != target.FileSizeBytes ||
						partSize != target.PartSizeBytes || partCount != target.PartCount || !slices.Equal(supplied, parts) {
						t.Fatal("合并未使用预检确认的原目标和有序回执")
					}
					if fixture.cancelAt == "complete" {
						cancel()
					}
					return metadata, fixture.completeErr
				},
				func(callContext context.Context, lease *domain.UploadTask, storedProvider, storedBucket, objectKey string, supplied domain.UploadObjectResult) (*domain.UploadTask, error) {
					calls = append(calls, "write")
					if callContext != ctx || lease != claimed || storedProvider != provider || storedBucket != bucket || objectKey != key ||
						!reflect.DeepEqual(&supplied, metadata) {
						t.Fatal("写回未沿用本轮原租约和存储确认")
					}
					if fixture.cancelAt == "write" {
						cancel()
					}
					return committed, fixture.writeErr
				},
				func(callContext context.Context, lease *domain.UploadTask, delay int) (*domain.UploadTask, error) {
					calls = append(calls, "retry")
					if callContext != ctx || lease != claimed || delay != fixture.wantDelay {
						t.Fatal("重试登记未使用原租约和预期退避", delay)
					}
					return scheduled, fixture.retryErr
				})

			// 3. 核对两条成功路径和失败边界。
			result, err := execution.Execute(ctx, claimed)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(calls, fixture.wantCalls) || !reflect.DeepEqual(claimed, original) {
				t.Fatalf("执行结果或调用顺序不符，得到 %v、%v，预期 %v、%v", err, calls, fixture.want, fixture.wantCalls)
			}
			if fixture.want == nil && result != committed || fixture.want != nil && result != nil {
				t.Fatal("执行返回的任务不符", result)
			}
			if fixture.name == "canceled with unknown result" && !errors.Is(err, context.Canceled) {
				t.Fatal("结果不明时丢失了取消原因", err)
			}
			var registered *UploadMergeRetryScheduledError
			wantRegistered := fixture.wantDelay > 0 && fixture.retryErr == nil && fixture.mode != "wrong retry"
			if errors.As(err, &registered) != wantRegistered || (wantRegistered && !registered.NextAttemptAt.Equal(*scheduled.NextAttemptAt)) {
				t.Fatal("重试登记结果或时间不符", err)
			}
			if fixture.retryErr != nil && !errors.Is(err, fixture.retryErr) || fixture.mode == "wrong retry" && !errors.Is(err, domain.ErrInvalidUploadTaskData) {
				t.Fatal("重试登记失败原因未保留", err)
			}
		})
	}
}
