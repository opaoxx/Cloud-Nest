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
	preflightRepair := []string{"preflight", "check repair", "reopen"}
	completionRepair := []string{"preflight", "complete", "check repair", "reopen"}
	failure := errors.New("fixture write failed")
	retryableWrite := errors.Join(domain.ErrUploadMergeWriteRetryable, failure)
	mismatch := &domain.UploadPartsMismatchError{PartNumbers: []uint32{2}}
	rejected := domain.ErrMultipartUploadPartRejected
	missing := domain.ErrUploadPartSessionMissing
	missingOnComplete := errors.Join(domain.ErrMultipartUploadCompletionUnknown, missing)
	preflightRecheck := []string{"preflight", "check missing"}
	completionRecheck := []string{"preflight", "complete", "check missing"}
	for _, fixture := range []struct {
		name, mode, cancelAt                string
		preflightErr, completeErr, writeErr error
		retryErr                            error
		checkRepairErr, reopenErr           error
		missingCheckErr                     error
		wantRepair                          bool
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
		{name: "preflight session loss recovers object", preflightErr: missing,
			wantCalls: []string{"preflight", "check missing", "write"}},
		{name: "completion session loss recovers object", completeErr: missingOnComplete,
			wantCalls: []string{"preflight", "complete", "check missing", "write"}},
		{name: "both missing schedules retry", preflightErr: missing, mode: "both missing",
			want: missing, wantCalls: []string{"preflight", "check missing", "retry"}, wantDelay: 1},
		{name: "session remains readable schedules retry", completeErr: missingOnComplete, missingCheckErr: errUploadMergeSessionPresent,
			want: missing, wantCalls: []string{"preflight", "complete", "check missing", "retry"}, wantDelay: 1},
		{name: "missing session check outage schedules retry", completeErr: missingOnComplete, missingCheckErr: domain.ErrUploadStorageUnavailable,
			want: missing, wantCalls: []string{"preflight", "complete", "check missing", "retry"}, wantDelay: 1},
		{name: "missing session check loses lease", preflightErr: missing, missingCheckErr: domain.ErrUploadTaskBusy,
			want: missing, wantCalls: preflightRecheck},
		{name: "missing session check target changed", preflightErr: missing, missingCheckErr: domain.ErrUploadTaskNotReady,
			want: missing, wantCalls: preflightRecheck},
		{name: "missing session check canceled", completeErr: missingOnComplete, cancelAt: "check missing",
			want: context.Canceled, wantCalls: completionRecheck},
		{name: "missing session check returns neither result", preflightErr: missing, mode: "no missing result",
			want: domain.ErrInvalidUploadTaskData, wantCalls: preflightRecheck},
		{name: "missing session check recovers different key", completeErr: missingOnComplete, mode: "wrong recovery key",
			want: domain.ErrInvalidUploadTaskData, wantCalls: completionRecheck},
		{name: "storage deadline caps backoff", completeErr: context.DeadlineExceeded,
			want: context.DeadlineExceeded, wantCalls: completionRetry, attemptCount: 10, wantDelay: 30},
		{name: "known lock failure schedules retry", writeErr: retryableWrite, want: failure, wantCalls: writeRetry, wantDelay: 1},
		{name: "retry write failure retains both errors", completeErr: domain.ErrMultipartUploadCompletionUnknown, retryErr: domain.ErrUploadTaskBusy,
			want: domain.ErrMultipartUploadCompletionUnknown, wantCalls: completionRetry, wantDelay: 1},
		{name: "invalid retry result retains original error", mode: "wrong retry", completeErr: domain.ErrMultipartUploadCompletionUnknown,
			want: domain.ErrMultipartUploadCompletionUnknown, wantCalls: completionRetry, wantDelay: 1},
		{name: "preflight mismatch reopens for repair", preflightErr: mismatch, wantRepair: true, wantCalls: preflightRepair},
		{name: "explicit rejection reopens for repair", completeErr: rejected, wantRepair: true, wantCalls: completionRepair},
		{name: "object appeared after mismatch", preflightErr: mismatch, checkRepairErr: domain.ErrUploadObjectExists,
			want: domain.ErrUploadObjectExists, wantCalls: preflightRepair[:2]},
		{name: "session vanished after rejection", completeErr: rejected, checkRepairErr: missing, mode: "both missing",
			want: missing, wantCalls: []string{"preflight", "complete", "check repair", "check missing", "retry"}, wantDelay: 1},
		{name: "repair check is inconclusive", preflightErr: mismatch, checkRepairErr: domain.ErrUploadStorageUnavailable,
			want: domain.ErrUploadStorageUnavailable, wantCalls: preflightRepair[:2]},
		{name: "parts match again", preflightErr: mismatch, checkRepairErr: domain.ErrUploadTaskNotReady,
			want: domain.ErrUploadTaskNotReady, wantCalls: preflightRepair[:2]},
		{name: "no repair evidence", preflightErr: mismatch, mode: "no evidence",
			want: domain.ErrInvalidUploadTaskData, wantCalls: preflightRepair[:2]},
		{name: "repair evidence has new lease version", preflightErr: mismatch, mode: "new version",
			want: domain.ErrInvalidUploadTaskData, wantCalls: preflightRepair[:2]},
		{name: "repair evidence has different lease deadline", preflightErr: mismatch, mode: "new deadline",
			want: domain.ErrInvalidUploadTaskData, wantCalls: preflightRepair[:2]},
		{name: "repair evidence has different session", preflightErr: mismatch, mode: "new session",
			want: domain.ErrInvalidUploadTaskData, wantCalls: preflightRepair[:2]},
		{name: "repair evidence names a different object", completeErr: rejected, mode: "wrong repair key",
			want: domain.ErrInvalidUploadTaskData, wantCalls: completionRepair[:3]},
		{name: "repair loses lease before commit", preflightErr: mismatch, reopenErr: domain.ErrUploadTaskBusy,
			want: domain.ErrUploadTaskBusy, wantCalls: preflightRepair},
		{name: "repair returns unrelated task", completeErr: rejected, mode: "wrong reopened task",
			want: domain.ErrInvalidUploadTaskData, wantCalls: completionRepair},
		{name: "canceled with mismatch", preflightErr: mismatch, cancelAt: "preflight",
			want: context.Canceled, wantCalls: preflightRepair[:1]},
		{name: "canceled after repair check", completeErr: rejected, cancelAt: "check repair",
			want: context.Canceled, wantCalls: completionRepair[:3]},
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
			reopened := copyUploadTask(committed)
			reopened.Status = domain.UploadStatusUploading
			evidence := &domain.UploadMergeRepairEvidence{Task: *copyUploadTask(claimed), ObjectKey: key, PartNumbers: []uint32{1}}
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
			missingResult := &UploadMergeMissingSessionResult{Recovered: &UploadMergeRecoveryObservation{Target: target, Object: *metadata}}
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
			case "no evidence":
				evidence = nil
			case "new version":
				evidence.Task.LeaseVersion++
			case "new deadline":
				evidence.Task.LeaseUntil = new(evidence.Task.LeaseUntil.Add(time.Second))
			case "new session":
				evidence.Task.StorageUploadID = new("different")
			case "wrong repair key":
				evidence.ObjectKey += ".other"
			case "wrong reopened task":
				reopened.ID++
			case "both missing":
				missingResult = &UploadMergeMissingSessionResult{Missing: &domain.UploadMergeMissingSessionObservation{Task: *copyUploadTask(claimed), ObjectKey: key}}
			case "no missing result":
				missingResult.Recovered = nil
			case "wrong recovery key":
				missingResult.Recovered.Target.ObjectKey += ".other"
			}
			repairCause := fixture.preflightErr
			if repairCause == nil {
				repairCause = fixture.completeErr
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
				},
				func(callContext context.Context, lease *domain.UploadTask, cause error) (*domain.UploadMergeRepairEvidence, error) {
					calls = append(calls, "check repair")
					if callContext != ctx || lease != claimed || cause != repairCause {
						t.Fatal("修复核对未使用当前租约及实际失配原因")
					}
					if fixture.cancelAt == "check repair" {
						cancel()
					}
					return evidence, fixture.checkRepairErr
				},
				func(callContext context.Context, storedProvider, storedBucket string, supplied *domain.UploadMergeRepairEvidence) (*domain.UploadTask, error) {
					calls = append(calls, "reopen")
					if callContext != ctx || storedProvider != provider || storedBucket != bucket || supplied != evidence {
						t.Fatal("修复写回未使用原配置和本轮核对证据")
					}
					return reopened, fixture.reopenErr
				},
				func(callContext context.Context, lease *domain.UploadTask, cause error) (*UploadMergeMissingSessionResult, error) {
					calls = append(calls, "check missing")
					if callContext != ctx || lease != claimed || !errors.Is(cause, missing) || !errors.Is(cause, repairCause) {
						t.Fatal("会话复核未使用原租约和会话缺失原因")
					}
					if fixture.cancelAt == "check missing" {
						cancel()
					}
					return missingResult, fixture.missingCheckErr
				})

			// 3. 核对成功状态和失败边界，包括原始失配及后续错误。
			result, err := execution.Execute(ctx, claimed)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(calls, fixture.wantCalls) || !reflect.DeepEqual(claimed, original) {
				t.Fatalf("执行结果或调用顺序不符，得到 %v、%v，预期 %v、%v", err, calls, fixture.want, fixture.wantCalls)
			}
			switch {
			case fixture.want != nil:
				if result != nil {
					t.Fatal("失败时返回了任务", result)
				}
			case fixture.wantRepair:
				if result != reopened {
					t.Fatal("失配后未返回已提交的补传任务", result)
				}
			default:
				if result != committed {
					t.Fatal("正常合并后未返回已提交的校验任务", result)
				}
			}
			if fixture.want != nil && (repairCause == mismatch || repairCause == rejected) && !errors.Is(err, repairCause) {
				t.Fatal("修复未成功时丢失了原失配原因", err)
			}
			if fixture.missingCheckErr != nil && !errors.Is(err, fixture.missingCheckErr) {
				t.Fatal("会话复核失败时丢失了本轮错误", err)
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
