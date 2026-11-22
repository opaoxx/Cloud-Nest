package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadSessionCandidateCleanupObservesOriginalSession(t *testing.T) {
	target, sessions := sessionCandidateFixture("Original+A/= ")
	session := sessions[0]
	closed := &domain.MultipartUploadAbortObservation{}
	version := "fixture-version"
	withObject := &domain.MultipartUploadAbortObservation{Object: &domain.UploadObjectResult{SizeBytes: 42, StorageVersionID: &version}}
	probeFailure := errors.New("fixture probe failure")
	abortFailure := errors.New("fixture lost abort response")
	readFailure := errors.New("fixture observation failure")
	afterAbort := []string{"probe", "lock", "abort", "observe"}
	for _, fixture := range []struct {
		name                              string
		missing, bound                    bool
		probeError, abortError, readError error
		cancelAt                          string
		observation, want                 *domain.MultipartUploadAbortObservation
		outcome                           MultipartUploadSessionAbortOutcome
		wantErrors                        []error
		wantCalls                         []string
	}{
		{name: "accepted and absent", observation: closed, want: closed, outcome: MultipartUploadSessionAbortAccepted, wantCalls: afterAbort},
		{name: "already missing with an object", missing: true, observation: withObject, want: withObject,
			outcome: MultipartUploadSessionAbortMissing, wantCalls: []string{"probe", "observe"}},
		{name: "missing between probe and abort", abortError: domain.ErrUploadPartSessionMissing, observation: closed, want: closed,
			outcome: MultipartUploadSessionAbortMissing, wantCalls: afterAbort},
		{name: "unknown instance or open session", outcome: MultipartUploadSessionAbortAccepted, wantCalls: afterAbort},
		{name: "response lost but closure observed", abortError: abortFailure, observation: closed, want: closed,
			outcome: MultipartUploadSessionAbortUnconfirmed, wantErrors: []error{abortFailure}, wantCalls: afterAbort},
		{name: "both stages fail", abortError: abortFailure, readError: readFailure, observation: closed, outcome: MultipartUploadSessionAbortUnconfirmed,
			wantErrors: []error{abortFailure, readFailure}, wantCalls: afterAbort},
		{name: "cancel during abort", cancelAt: "abort", abortError: abortFailure, outcome: MultipartUploadSessionAbortUnconfirmed,
			wantErrors: []error{abortFailure, context.Canceled}, wantCalls: []string{"probe", "lock", "abort"}},
		{name: "cancel during observation", cancelAt: "observe", readError: readFailure, observation: closed, outcome: MultipartUploadSessionAbortAccepted,
			wantErrors: []error{readFailure, context.Canceled}, wantCalls: afterAbort},
		{name: "only cancellation during observation", cancelAt: "observe", outcome: MultipartUploadSessionAbortAccepted,
			wantErrors: []error{context.Canceled}, wantCalls: afterAbort},
		{name: "protected", bound: true, outcome: MultipartUploadSessionAbortSkipped, wantCalls: []string{"probe", "lock"}},
		{name: "probe failed", probeError: probeFailure, wantErrors: []error{probeFailure}, wantCalls: []string{"probe"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls []string
			rechecker := NewMultipartUploadSessionCandidateRechecker("local_minio", "fixture-bucket", func(context.Context, string, string) (bool, error) {
				calls = append(calls, "probe")
				return !fixture.missing, fixture.probeError
			}, func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionLockedCheck, error) {
				calls = append(calls, "lock")
				return domain.MultipartUploadSessionLockedCheck{TaskStatus: domain.UploadStatusUploading, Facts: domain.MultipartUploadSessionCleanupFacts{
					GraceElapsed: true, Protection: domain.MultipartUploadSessionProtection{Bound: fixture.bound},
				}}, nil
			})
			aborter := NewMultipartUploadSessionCandidateAborter(rechecker, func(context.Context, string, string) error {
				calls = append(calls, "abort")
				if fixture.cancelAt == "abort" {
					cancel()
				}
				return fixture.abortError
			})
			cleanup := NewMultipartUploadSessionCandidateCleanup(aborter, func(callContext context.Context, key, uploadID string) (*domain.MultipartUploadAbortObservation, error) {
				calls = append(calls, "observe")
				if callContext != ctx || key != session.ObjectKey || uploadID != session.UploadID {
					t.Fatal("只读复查未使用原会话定位")
				}
				if fixture.cancelAt == "observe" {
					cancel()
				}
				return fixture.observation, fixture.readError
			})
			result, err := cleanup.Execute(ctx, target, session, time.Minute)
			if result.AbortOutcome != fixture.outcome || result.Observation != fixture.want || !slices.Equal(calls, fixture.wantCalls) ||
				(err == nil) != (len(fixture.wantErrors) == 0) {
				t.Fatal("收尾复查结果或调用顺序不符", result, calls, err)
			}
			for _, expectedError := range fixture.wantErrors {
				if !errors.Is(err, expectedError) {
					t.Fatal("未保留失败原因", expectedError, err)
				}
			}
			if fixture.cancelAt == "observe" && fixture.readError == nil && err != context.Canceled {
				t.Fatal("纯取消信号被包装成合并故障", err)
			}
		})
	}
}
