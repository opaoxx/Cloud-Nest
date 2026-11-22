package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadSessionCleanupRoundResumesPendingCandidates(t *testing.T) {
	first, second, _ := sessionBatchTargets()
	const grace = time.Minute
	limits := MultipartUploadSessionCleanupLimits{Objects: 2, SessionsPerPage: 3, StoragePages: 2, Candidates: 2}
	fixtureFailure := errors.New("fixture abort failure")
	var listCalls int
	var attempts []string
	var stopCandidate context.CancelFunc
	pageScanner := NewMultipartUploadSessionCandidateScanner("local_minio", "fixture-bucket",
		func(_ context.Context, key, _ string, _ int) (*domain.MultipartUploadSessionPage, error) {
			names := []string{"cancel", "last"}
			if key == first.ObjectKey {
				names = []string{"closed", "protected", "failed"}
			}
			page := &domain.MultipartUploadSessionPage{}
			for _, id := range names {
				page.Sessions = append(page.Sessions, domain.MultipartUploadSession{ObjectKey: key, UploadID: id, InitiatedAt: time.Unix(1, 0)})
			}
			return page, nil
		}, func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionCleanupFacts, error) {
			return domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true}, nil
		})
	scanner := NewMultipartUploadSessionCandidateBatchScanner(pageScanner, func(context.Context, string, string, uint64, int) (*domain.MultipartUploadSessionScanPage, error) {
		listCalls++
		return &domain.MultipartUploadSessionScanPage{Targets: []domain.MultipartUploadSessionScanTarget{first, second}}, nil
	})
	rechecker := NewMultipartUploadSessionCandidateRechecker("local_minio", "fixture-bucket", func(_ context.Context, _, id string) (bool, error) {
		attempts = append(attempts, id)
		if id == "cancel" && stopCandidate != nil {
			stopCandidate()
		}
		return true, nil
	}, func(_ context.Context, _, _ string, _ domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, _ time.Duration) (domain.MultipartUploadSessionLockedCheck, error) {
		return domain.MultipartUploadSessionLockedCheck{TaskStatus: domain.UploadStatusUploading, Facts: domain.MultipartUploadSessionCleanupFacts{
			GraceElapsed: true, Protection: domain.MultipartUploadSessionProtection{Bound: session.UploadID == "protected"},
		}}, nil
	})
	aborter := NewMultipartUploadSessionCandidateAborter(rechecker, func(_ context.Context, _, id string) error {
		if id == "failed" {
			return fixtureFailure
		}
		return nil
	})
	cleanup := NewMultipartUploadSessionCandidateCleanup(aborter, func(_ context.Context, _, id string) (*domain.MultipartUploadAbortObservation, error) {
		if id == "failed" || id == "last" {
			return nil, nil
		}
		return &domain.MultipartUploadAbortObservation{}, nil
	})
	round := NewMultipartUploadSessionCleanupRound(scanner, cleanup)

	initial, err := round.RunOnce(t.Context(), MultipartUploadSessionCleanupPosition{}, limits, grace)
	if err != nil || !initial.Scan.Done || initial.Done || initial.Scan.PagesScanned != 2 || initial.Confirmed != 1 ||
		initial.Skipped != 1 || len(initial.Next.pending) != 3 || len(initial.Items) != 2 ||
		initial.Items[0].Target != first || initial.Items[1].Session.UploadID != "protected" {
		t.Fatal("达到候选上限时未保存本批其余候选", initial, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopCandidate = cancel
	interrupted, err := round.RunOnce(ctx, initial.Next, limits, grace)
	if !errors.Is(err, context.Canceled) || interrupted.Done || len(interrupted.Items) != 2 ||
		interrupted.Unconfirmed != 1 || interrupted.Failed != 2 || !errors.Is(interrupted.Items[0].Err, fixtureFailure) ||
		len(interrupted.Next.pending) != 2 || interrupted.Items[1].Target != second {
		t.Fatal("逐项故障或取消时未保留相应结果和位置", interrupted, err)
	}
	stopCandidate = nil
	finished, err := round.RunOnce(t.Context(), interrupted.Next, limits, grace)
	if err != nil || !finished.Done || finished.Confirmed != 1 || finished.Unconfirmed != 1 || finished.Failed != 0 || listCalls != 1 ||
		!slices.Equal(attempts, []string{"closed", "protected", "failed", "cancel", "cancel", "last"}) {
		t.Fatal("续处理跳过了候选或重新扫描了已缓存的批次", finished, attempts, listCalls, err)
	}
}
