package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadSessionCandidateScannerClassifiesOnePage(t *testing.T) {
	ctx := t.Context()
	target, sessions := sessionCandidateFixture("bound", "creating", "recent", "failed", "candidate")
	const gracePeriod = 15 * time.Minute
	lookupFailure := errors.New("fixture lookup failure")
	listCalls := 0
	var checked []string
	scanner := NewMultipartUploadSessionCandidateScanner("local_minio", "fixture-bucket", func(callContext context.Context, key, marker string, limit int) (*domain.MultipartUploadSessionPage, error) {
		listCalls++
		if callContext != ctx || key != target.ObjectKey || marker != "prior" || limit != 5 {
			t.Fatal("分页参数不符")
		}
		return &domain.MultipartUploadSessionPage{Sessions: sessions, NextUploadID: "candidate"}, nil
	}, func(callContext context.Context, provider, bucket string, gotTarget domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, grace time.Duration) (domain.MultipartUploadSessionCleanupFacts, error) {
		checked = append(checked, session.UploadID)
		if callContext != ctx || provider != "local_minio" || bucket != "fixture-bucket" || gotTarget != target || grace != gracePeriod {
			t.Fatal("仓储复核参数不符")
		}
		switch session.UploadID {
		case "bound":
			return domain.MultipartUploadSessionCleanupFacts{Protection: domain.MultipartUploadSessionProtection{Bound: true}}, nil
		case "creating":
			return domain.MultipartUploadSessionCleanupFacts{Protection: domain.MultipartUploadSessionProtection{ActiveCreationLease: true}, GraceElapsed: true}, nil
		case "recent":
			return domain.MultipartUploadSessionCleanupFacts{}, nil
		case "failed":
			return domain.MultipartUploadSessionCleanupFacts{}, lookupFailure
		default:
			return domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true}, nil
		}
	})
	got, err := scanner.ScanPage(ctx, target, "prior", 5, gracePeriod)
	want := MultipartUploadSessionCandidatePage{Scanned: 5, Candidates: sessions[4:], SkippedBound: 1,
		SkippedCreation: 1, SkippedWithinGrace: 1, NextUploadID: "candidate",
		Failures: []MultipartUploadSessionScanFailure{{UploadID: "failed", Err: lookupFailure}}}
	if err != nil || listCalls != 1 || !reflect.DeepEqual(got, want) ||
		!reflect.DeepEqual(checked, []string{"bound", "creating", "recent", "failed", "candidate"}) {
		t.Fatal("扫描结果、游标或逐条错误不符", got, checked, err)
	}
}

func TestMultipartUploadSessionCandidateScannerRejectsMalformedPage(t *testing.T) {
	target, sessions := sessionCandidateFixture("first", "second")
	for _, fixture := range []struct {
		name string
		edit func(*domain.MultipartUploadSessionPage)
	}{
		{"duplicate", func(page *domain.MultipartUploadSessionPage) { page.Sessions[1] = page.Sessions[0] }},
		{"wrong key", func(page *domain.MultipartUploadSessionPage) { page.Sessions[1].ObjectKey = "other" }},
		{"repeated incoming marker", func(page *domain.MultipartUploadSessionPage) { page.Sessions[1].UploadID = "prior" }},
		{"cursor not in page", func(page *domain.MultipartUploadSessionPage) { page.NextUploadID = "other" }},
		{"oversized", func(page *domain.MultipartUploadSessionPage) { page.Sessions = append(page.Sessions, page.Sessions[0]) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			scanner := NewMultipartUploadSessionCandidateScanner("local_minio", "fixture-bucket", func(context.Context, string, string, int) (*domain.MultipartUploadSessionPage, error) {
				page := &domain.MultipartUploadSessionPage{Sessions: append([]domain.MultipartUploadSession(nil), sessions...)}
				fixture.edit(page)
				return page, nil
			}, func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionCleanupFacts, error) {
				t.Fatal("异常页触发了仓储复核")
				return domain.MultipartUploadSessionCleanupFacts{}, nil
			})
			got, err := scanner.ScanPage(t.Context(), target, "prior", 2, time.Minute)
			if !errors.Is(err, domain.ErrUploadStorageUnavailable) || !reflect.DeepEqual(got, MultipartUploadSessionCandidatePage{}) {
				t.Fatal("异常页未被拒绝", got, err)
			}
		})
	}
}

func TestMultipartUploadSessionCandidateScannerStopsWithoutAdvancing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	target, sessions := sessionCandidateFixture("first", "second", "third")
	lookupFailure := errors.New("fixture independent fault")
	scanner := NewMultipartUploadSessionCandidateScanner("local_minio", "fixture-bucket", func(context.Context, string, string, int) (*domain.MultipartUploadSessionPage, error) {
		return &domain.MultipartUploadSessionPage{Sessions: sessions, NextUploadID: "third"}, nil
	}, func(_ context.Context, _, _ string, _ domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, _ time.Duration) (domain.MultipartUploadSessionCleanupFacts, error) {
		switch session.UploadID {
		case "first":
			return domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true}, nil
		case "second":
			cancel()
			return domain.MultipartUploadSessionCleanupFacts{}, errors.Join(ctx.Err(), lookupFailure)
		default:
			t.Fatal("取消后仍然调用了仓储")
			return domain.MultipartUploadSessionCleanupFacts{}, nil
		}
	})
	got, err := scanner.ScanPage(ctx, target, "", 3, time.Minute)
	if !errors.Is(err, context.Canceled) || got.Scanned != 3 || !reflect.DeepEqual(got.Candidates, sessions[:1]) || got.NextUploadID != "" ||
		len(got.Failures) != 1 || got.Failures[0].UploadID != "second" || !errors.Is(got.Failures[0].Err, lookupFailure) {
		t.Fatal("取消时未保留已完成读取和独立故障", got, err)
	}
}

func sessionCandidateFixture(ids ...string) (domain.MultipartUploadSessionScanTarget, []domain.MultipartUploadSession) {
	const key = "users/7/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e25"
	target := domain.MultipartUploadSessionScanTarget{ObjectID: 23, UserID: 7, TaskID: 19, ObjectKey: key}
	sessions := make([]domain.MultipartUploadSession, 0, len(ids))
	for _, id := range ids {
		sessions = append(sessions, domain.MultipartUploadSession{ObjectKey: key, UploadID: id, InitiatedAt: time.Unix(1, 0).UTC()})
	}
	return target, sessions
}
