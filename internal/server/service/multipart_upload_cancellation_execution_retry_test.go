package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadCancellationExecutionRetry(t *testing.T) {
	storageCause := domain.ErrUploadStorageUnavailable
	writeCause := domain.ErrUploadCancellationRetryable
	storageReason, databaseReason := domain.MultipartCancellationRetryStorage, domain.MultipartCancellationRetryDatabase
	busy := domain.ErrUploadTaskBusy
	invalid := domain.ErrInvalidUploadTaskData
	for _, fixture := range []struct {
		name, cancelAt                          string
		attempts                                uint32
		delay, elapsed, next                    int
		reason                                  domain.MultipartUploadCancellationRetryReason
		observeError, writeError, scheduleError error
		wantCause, wantOther                    error
		pending, missingWriteback               bool
		edit                                    func(*domain.UploadTask)
	}{
		{name: "pending one second", pending: true, reason: storageReason, delay: 1, next: 60, wantCause: storageCause},
		{name: "storage two seconds", attempts: 1, observeError: storageCause, reason: storageReason, delay: 2, next: 60, wantCause: storageCause},
		{name: "database four seconds", attempts: 2, writeError: writeCause, reason: databaseReason, delay: 4, next: 60, wantCause: writeCause},
		{name: "storage eight seconds", attempts: 3, observeError: storageCause, reason: storageReason, delay: 8, next: 60, wantCause: storageCause},
		{name: "storage sixteen seconds", attempts: 4, observeError: storageCause, reason: storageReason, delay: 16, elapsed: 50, next: 66, wantCause: storageCause},
		{name: "database thirty seconds", attempts: 5, writeError: writeCause, reason: databaseReason, delay: 30, elapsed: 50, next: 80, wantCause: writeCause},
		{name: "backoff is capped", attempts: 50, observeError: storageCause, reason: storageReason, delay: 30, elapsed: 55, next: 85, wantCause: storageCause},
		{name: "confirmed storage timeout", observeError: errors.Join(storageCause, context.DeadlineExceeded), reason: storageReason, delay: 1, next: 60, wantCause: storageCause, wantOther: context.DeadlineExceeded},
		{name: "unclassified lookup timeout", observeError: context.DeadlineExceeded, wantCause: context.DeadlineExceeded},
		{name: "old lease", observeError: busy, wantCause: busy},
		{name: "canceled storage call", observeError: errors.Join(storageCause, context.Canceled), wantCause: storageCause, wantOther: context.Canceled},
		{name: "caller canceled before scheduling", observeError: storageCause, cancelAt: "observe", wantCause: storageCause, wantOther: context.Canceled},
		{name: "caller canceled after commit", writeError: writeCause, cancelAt: "schedule", reason: databaseReason, delay: 1, next: 60, wantCause: writeCause},
		{name: "failed registration retains both causes", writeError: writeCause, reason: databaseReason, delay: 1, scheduleError: busy, wantCause: writeCause, wantOther: busy},
		{name: "empty registration result", pending: true, reason: storageReason, delay: 1, missingWriteback: true, wantCause: storageCause, wantOther: invalid},
		{name: "wrong retry time", pending: true, reason: storageReason, delay: 1, next: 60,
			edit: func(task *domain.UploadTask) { task.NextAttemptAt = new(task.NextAttemptAt.Add(-time.Second)) }, wantCause: storageCause, wantOther: invalid},
		{name: "different retry binding", pending: true, reason: storageReason, delay: 1, next: 60,
			edit: func(task *domain.UploadTask) { task.StorageUploadID = new("replacement") }, wantCause: storageCause, wantOther: invalid},
		{name: "reservation drift", pending: true, reason: storageReason, delay: 1, next: 60,
			edit: func(task *domain.UploadTask) { task.ReservedBytes = 0 }, wantCause: storageCause, wantOther: invalid},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			start := time.Now().UTC().Truncate(time.Second)
			claimed := &domain.UploadTask{ID: 19, UserID: 7, StagedObjectID: 23, UploadMode: "MULTIPART",
				Status: domain.UploadStatusCancelling, LeaseOwner: new("retry-worker"), LeaseVersion: 5, LeaseUntil: new(start.Add(time.Minute)),
				ExpiresAt: start.Add(-time.Minute), UpdatedAt: start, AttemptCount: fixture.attempts, ReservedBytes: 16777217}
			before := copyUploadTask(claimed)
			var observations, writes, retries int

			// 1. 准备单次观察、终态写回和重试登记，核对调用顺序与原租约。
			observe := func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCancellationObservation, error) {
				observations++
				if fixture.cancelAt == "observe" {
					cancel()
				}
				if fixture.pending || fixture.observeError != nil {
					return nil, fixture.observeError
				}
				return &domain.MultipartUploadCancellationObservation{Task: *copyUploadTask(claimed), ObjectKey: "fixture/original"}, nil
			}
			complete := func(context.Context, string, string, *domain.MultipartUploadCancellationObservation) (*domain.UploadTask, error) {
				writes++
				return nil, fixture.writeError
			}
			schedule := func(callContext context.Context, task *domain.UploadTask, delay int, reason domain.MultipartUploadCancellationRetryReason) (*domain.UploadTask, error) {
				retries++
				if observations != 1 || callContext != ctx || callContext.Err() != nil || !reflect.DeepEqual(task, before) ||
					delay != fixture.delay || reason != fixture.reason {
					t.Fatal("登记时的时间、原因或原任务不符", delay, reason)
				}
				if fixture.cancelAt == "schedule" {
					cancel()
				}
				if fixture.scheduleError != nil || fixture.missingWriteback {
					return nil, fixture.scheduleError
				}
				updated := copyUploadTask(claimed)
				updated.LeaseOwner, updated.LeaseUntil = nil, nil
				updated.AttemptCount++
				updated.UpdatedAt = start.Add(time.Duration(fixture.elapsed) * time.Second)
				updated.NextAttemptAt = new(start.Add(time.Duration(fixture.next) * time.Second))
				updated.LastErrorCode = new(string(reason))
				if fixture.edit != nil {
					fixture.edit(updated)
				}
				return updated, nil
			}
			execution := NewMultipartUploadCancellationExecutionService("local_minio", "fixture-bucket",
				observe, observe, complete, complete, schedule)

			// 2. 核对登记只发生一次，固定错误或无效写回没有登记成功信号。
			finished, err := execution.Execute(ctx, claimed)
			wantRetry := fixture.reason != ""
			if finished != nil || observations != 1 || (writes == 1) != (fixture.writeError != nil) ||
				retries > 1 || (retries == 1) != wantRetry || !errors.Is(err, fixture.wantCause) ||
				(fixture.wantOther != nil && !errors.Is(err, fixture.wantOther)) || !reflect.DeepEqual(before, claimed) {
				t.Fatal("重试次数、原始错误或原租约不符", observations, writes, retries, err)
			}
			var scheduled *MultipartUploadCancellationRetryScheduledError
			wantSignal := wantRetry && fixture.scheduleError == nil && fixture.edit == nil && !fixture.missingWriteback
			if errors.As(err, &scheduled) != wantSignal {
				t.Fatal("登记成功信号与实际写回不符", err)
			}
			if wantSignal && (scheduled == nil || !scheduled.NextAttemptAt.Equal(start.Add(time.Duration(fixture.next)*time.Second))) {
				t.Fatal("返回的下次尝试时间不符", scheduled)
			}
		})
	}
}
