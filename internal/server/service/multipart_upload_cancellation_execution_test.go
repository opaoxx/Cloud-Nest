package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadCancellationExecution(t *testing.T) {
	testMultipartUploadCleanupExecution(t, domain.UploadStatusCancelling, domain.UploadStatusCancelled)
}

func TestMultipartUploadExpirationExecution(t *testing.T) {
	testMultipartUploadCleanupExecution(t, domain.UploadStatusExpiring, domain.UploadStatusExpired)
}

func testMultipartUploadCleanupExecution(t *testing.T, pendingStatus, terminalStatus domain.UploadStatus) {
	t.Helper()
	const provider, bucket, key, session = "local_minio", "fixture-bucket", "fixture/original +%.bin", "Fixture+A/= "
	const size = domain.MaxSingleUploadSizeBytes + 1
	invalid := domain.ErrInvalidUploadTaskData
	otherPending, otherTerminal := domain.UploadStatusExpiring, domain.UploadStatusExpired
	if pendingStatus == domain.UploadStatusExpiring {
		otherPending, otherTerminal = domain.UploadStatusCancelling, domain.UploadStatusCancelled
	}
	type executionFixture struct {
		name, calls, cancelAt             string
		bound, withObject                 bool
		observeError, completeError, want error
		wantTask, missingResult           bool
		editClaim, editResult             func(*domain.UploadTask)
		editObservation                   func(*domain.MultipartUploadCleanupObservation)
	}
	fixtures := []executionFixture{
		{name: "bound and missing", bound: true, calls: "bound missing", wantTask: true},
		{name: "bound and existing", bound: true, withObject: true, calls: "bound existing", wantTask: true},
		{name: "unbound and missing", calls: "unbound missing", wantTask: true},
		{name: "unbound and existing", withObject: true, calls: "unbound existing", wantTask: true},
		{name: "unexpected target lookup error", calls: "unbound", observeError: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "lease changed after observation", bound: true, calls: "bound",
			editObservation: func(observed *domain.MultipartUploadCleanupObservation) { observed.Task.LeaseVersion++ }, want: invalid},
		{name: "observation of other account", calls: "unbound",
			editObservation: func(observed *domain.MultipartUploadCleanupObservation) { observed.Task.UserID++ }, want: invalid},
		{name: "session trimmed", bound: true, calls: "bound",
			editObservation: func(observed *domain.MultipartUploadCleanupObservation) {
				observed.Task.StorageUploadID = new(strings.TrimSpace(session))
			}, want: invalid},
		{name: "bound in unbound observation", calls: "unbound",
			editObservation: func(observed *domain.MultipartUploadCleanupObservation) {
				observed.Task.StorageUploadID = new(session)
			}, want: invalid},
		{name: "empty observation key", calls: "unbound",
			editObservation: func(observed *domain.MultipartUploadCleanupObservation) { observed.ObjectKey = "" }, want: invalid},
		{name: "database rejects old lease", calls: "unbound missing", completeError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy},
		{name: "canceled before first call", cancelAt: "start", want: context.Canceled},
		{name: "canceled during observation", cancelAt: "observe", calls: "unbound", want: context.Canceled},
		{name: "canceled after commit", cancelAt: "complete", calls: "unbound missing", wantTask: true},
		{name: "invalid claim session", bound: true, editClaim: func(task *domain.UploadTask) { task.StorageUploadID = new(" ") }, want: invalid},
		{name: "invalid claim phase", editClaim: func(task *domain.UploadTask) { task.Status = otherPending }, want: invalid},
		{name: "nil success result from transaction", calls: "unbound missing", missingResult: true, want: invalid},
		{name: "wrong returned task", calls: "unbound missing", editResult: func(task *domain.UploadTask) { task.ID++ }, want: invalid},
		{name: "wrong terminal status", calls: "unbound missing", editResult: func(task *domain.UploadTask) { task.Status = otherTerminal }, want: invalid},
		{name: "reservation not settled", calls: "unbound missing", editResult: func(task *domain.UploadTask) { task.ReservedBytes = 1 }, want: invalid},
		{name: "completion at lease deadline", calls: "unbound missing",
			editResult: func(task *domain.UploadTask) {
				task.UpdatedAt = task.UpdatedAt.Add(time.Minute - time.Second)
				task.CompletedAt = new(task.UpdatedAt)
			}, want: invalid},
	}
	if pendingStatus == domain.UploadStatusExpiring {
		fixtures = append(fixtures,
			executionFixture{name: "completion at upload deadline", calls: "unbound missing", wantTask: true,
				editClaim: func(task *domain.UploadTask) { task.ExpiresAt = task.UpdatedAt.Add(time.Second) }},
			executionFixture{name: "completion before upload deadline", calls: "unbound missing", want: invalid,
				editClaim: func(task *domain.UploadTask) { task.ExpiresAt = task.UpdatedAt.Add(2 * time.Second) }},
		)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			start := time.Now().UTC().Truncate(time.Second)
			claimed := &domain.UploadTask{ID: 19, UserID: 7, StagedObjectID: 23, UploadMode: "MULTIPART", Status: pendingStatus,
				FileSize: size, ReservedBytes: size, LeaseVersion: 5, AttemptCount: 2, LeaseOwner: new("fixture-worker"),
				LeaseUntil: new(start.Add(time.Minute)), ExpiresAt: start.Add(-time.Minute), UpdatedAt: start}
			if fixture.bound {
				claimed.StorageUploadID = new(session)
			}
			if fixture.editClaim != nil {
				fixture.editClaim(claimed)
			}
			var calls []string
			var observation *domain.MultipartUploadCleanupObservation
			var expected *domain.UploadTask
			// 1. 捕获观察和写回调用，以原任务和返回指针核对实际传参。
			observe := func(branch string, callContext context.Context, task *domain.UploadTask) (*domain.MultipartUploadCleanupObservation, error) {
				calls = append(calls, branch)
				if callContext != ctx || task != claimed {
					t.Fatal("观察未收到本轮领取的任务和上下文")
				}
				if fixture.cancelAt == "observe" {
					cancel()
				}
				if fixture.observeError != nil {
					return nil, fixture.observeError
				}
				observation = &domain.MultipartUploadCleanupObservation{Task: *copyUploadTask(claimed), ObjectKey: key}
				if fixture.withObject {
					observation.Object = &domain.UploadObjectResult{SizeBytes: 29, StorageETag: new("fixture-etag"), StorageVersionID: new("Version+A/=")}
				}
				if fixture.editObservation != nil {
					fixture.editObservation(observation)
				}
				return observation, nil
			}
			complete := func(branch string, callContext context.Context, gotProvider, gotBucket string, observed *domain.MultipartUploadCleanupObservation) (*domain.UploadTask, error) {
				calls = append(calls, branch)
				if callContext != ctx || gotProvider != provider || gotBucket != bucket || observed != observation {
					t.Fatal("写回未收到原观察、存储配置或上下文")
				}
				if fixture.cancelAt == "complete" {
					cancel()
				}
				if fixture.missingResult || fixture.completeError != nil {
					return nil, fixture.completeError
				}
				expected = copyUploadTask(claimed)
				expected.Status, expected.ReservedBytes = terminalStatus, 0
				expected.LeaseOwner, expected.LeaseUntil = nil, nil
				expected.UpdatedAt = start.Add(time.Second)
				expected.CompletedAt = new(expected.UpdatedAt)
				if fixture.editResult != nil {
					fixture.editResult(expected)
				}
				return expected, nil
			}
			observeBound := func(callContext context.Context, task *domain.UploadTask) (*domain.MultipartUploadCleanupObservation, error) {
				return observe("bound", callContext, task)
			}
			observeUnbound := func(callContext context.Context, task *domain.UploadTask) (*domain.MultipartUploadCleanupObservation, error) {
				return observe("unbound", callContext, task)
			}
			completeMissing := func(callContext context.Context, provider, bucket string, observed *domain.MultipartUploadCleanupObservation) (*domain.UploadTask, error) {
				return complete("missing", callContext, provider, bucket, observed)
			}
			completeExisting := func(callContext context.Context, provider, bucket string, observed *domain.MultipartUploadCleanupObservation) (*domain.UploadTask, error) {
				return complete("existing", callContext, provider, bucket, observed)
			}
			var execute func(context.Context, *domain.UploadTask) (*domain.UploadTask, error)
			unexpectedRetry := func(context.Context, *domain.UploadTask, int, domain.MultipartUploadCleanupRetryReason) (*domain.UploadTask, error) {
				t.Fatal("确认路径或不可重试错误调用了重试登记")
				return nil, nil
			}
			if pendingStatus == domain.UploadStatusExpiring {
				execute = NewMultipartUploadExpirationExecutionService(provider, bucket, observeBound, observeUnbound, completeMissing, completeExisting,
					unexpectedRetry).Execute
			} else {
				execute = NewMultipartUploadCancellationExecutionService(provider, bucket, observeBound, observeUnbound, completeMissing, completeExisting,
					unexpectedRetry).Execute
			}

			// 2. 核对实际路径和返回值，确保错误与待确认结果没有被当作终态。
			before := copyUploadTask(claimed)
			if fixture.cancelAt == "start" {
				cancel()
			}
			result, err := execute(ctx, claimed)
			if got := strings.Join(calls, " "); got != fixture.calls || !errors.Is(err, fixture.want) ||
				(result != nil) != fixture.wantTask || (fixture.wantTask && result != expected) || !reflect.DeepEqual(before, claimed) {
				t.Fatal("任务执行路径、结果或原快照不符", got, result, err)
			}
		})
	}
}
