package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadSessionCandidateAborterRequiresCurrentEligibility(t *testing.T) {
	target, sessions := sessionCandidateFixture("Candidate+A/= ")
	session := sessions[0]
	eligible := domain.MultipartUploadSessionLockedCheck{
		TaskStatus: domain.UploadStatusUploading,
		Facts:      domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true},
	}
	bound := eligible
	bound.Facts.Protection.Bound = true
	creating := eligible
	creating.TaskStatus = domain.UploadStatusCreating
	withinGrace := eligible
	withinGrace.Facts.GraceElapsed = false
	probeFailure := errors.New("fixture probe failure")
	lockFailure := errors.New("fixture lock failure")
	abortFailure := errors.New("fixture unconfirmed response")
	for _, fixture := range []struct {
		name       string
		exists     bool
		locked     domain.MultipartUploadSessionLockedCheck
		probeError error
		lockError  error
		abortError error
		cancelAt   string
		want       MultipartUploadSessionAbortOutcome
		wantError  error
		wantCalls  []string
	}{
		{name: "already missing", want: MultipartUploadSessionAbortMissing, wantCalls: []string{"probe"}},
		{name: "bound", exists: true, locked: bound, want: MultipartUploadSessionAbortSkipped, wantCalls: []string{"probe", "lock"}},
		{name: "creating without lease", exists: true, locked: creating, want: MultipartUploadSessionAbortSkipped, wantCalls: []string{"probe", "lock"}},
		{name: "within grace", exists: true, locked: withinGrace, want: MultipartUploadSessionAbortSkipped, wantCalls: []string{"probe", "lock"}},
		{name: "probe failed", probeError: probeFailure, wantError: probeFailure, wantCalls: []string{"probe"}},
		{name: "lock failed", exists: true, lockError: lockFailure, wantError: lockFailure, wantCalls: []string{"probe", "lock"}},
		{name: "canceled during recheck", exists: true, locked: eligible, cancelAt: "lock", wantError: context.Canceled, wantCalls: []string{"probe", "lock"}},
		{name: "accepted", exists: true, locked: eligible, want: MultipartUploadSessionAbortAccepted, wantCalls: []string{"probe", "lock", "abort"}},
		{name: "missing during abort", exists: true, locked: eligible, abortError: fmt.Errorf("fixture: %w", domain.ErrUploadPartSessionMissing),
			want: MultipartUploadSessionAbortMissing, wantCalls: []string{"probe", "lock", "abort"}},
		{name: "response unknown", exists: true, locked: eligible, abortError: abortFailure, want: MultipartUploadSessionAbortUnconfirmed,
			wantError: abortFailure, wantCalls: []string{"probe", "lock", "abort"}},
		{name: "canceled in flight", exists: true, locked: eligible, cancelAt: "abort", abortError: abortFailure, want: MultipartUploadSessionAbortUnconfirmed,
			wantError: context.Canceled, wantCalls: []string{"probe", "lock", "abort"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls []string
			rechecker := NewMultipartUploadSessionCandidateRechecker("local_minio", "fixture-bucket", func(context.Context, string, string) (bool, error) {
				calls = append(calls, "probe")
				return fixture.exists, fixture.probeError
			}, func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionLockedCheck, error) {
				calls = append(calls, "lock")
				if fixture.cancelAt == "lock" {
					cancel()
				}
				return fixture.locked, fixture.lockError
			})
			aborter := NewMultipartUploadSessionCandidateAborter(rechecker, func(callContext context.Context, key, uploadID string) error {
				calls = append(calls, "abort")
				if callContext != ctx || key != session.ObjectKey || uploadID != session.UploadID {
					t.Fatal("中止调用改变了原始会话定位")
				}
				if fixture.cancelAt == "abort" {
					cancel()
				}
				return fixture.abortError
			})
			got, err := aborter.AbortCandidate(ctx, target, session, time.Minute)
			if got != fixture.want || !errors.Is(err, fixture.wantError) || !slices.Equal(calls, fixture.wantCalls) {
				t.Fatal("单条中止结果或调用顺序不符", got, calls, err)
			}
			if fixture.cancelAt == "abort" && !errors.Is(err, abortFailure) {
				t.Fatal("中止取消时未保留独立存储错误", err)
			}
		})
	}
}
