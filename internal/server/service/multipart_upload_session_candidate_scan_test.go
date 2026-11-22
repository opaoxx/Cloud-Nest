package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadSessionCandidateScannerScanPagesStopsAtBudgetAndResumes(t *testing.T) {
	target, sessions := sessionCandidateFixture("bound", "creating", "recent", "broken", "candidate", "last")
	lookupFailure := errors.New("fixture lookup failure")
	var markers []string
	var checked []string
	scanner := NewMultipartUploadSessionCandidateScanner("local_minio", "fixture-bucket", func(_ context.Context, key, marker string, limit int) (*domain.MultipartUploadSessionPage, error) {
		if key != target.ObjectKey || limit != 2 {
			t.Fatal("跨页改变了对象键或页大小")
		}
		markers = append(markers, marker)
		switch marker {
		case "":
			return &domain.MultipartUploadSessionPage{Sessions: sessions[:2], NextUploadID: "creating"}, nil
		case "creating":
			return &domain.MultipartUploadSessionPage{Sessions: sessions[2:4], NextUploadID: "broken"}, nil
		case "broken":
			return &domain.MultipartUploadSessionPage{Sessions: sessions[4:]}, nil
		default:
			t.Fatalf("意外游标 %q", marker)
			return nil, nil
		}
	}, func(_ context.Context, _, _ string, gotTarget domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, grace time.Duration) (domain.MultipartUploadSessionCleanupFacts, error) {
		if gotTarget != target || grace != time.Minute {
			t.Fatal("跨页改变了目标或宽限期")
		}
		checked = append(checked, session.UploadID)
		switch session.UploadID {
		case "bound":
			return domain.MultipartUploadSessionCleanupFacts{Protection: domain.MultipartUploadSessionProtection{Bound: true}}, nil
		case "creating":
			return domain.MultipartUploadSessionCleanupFacts{Protection: domain.MultipartUploadSessionProtection{ActiveCreationLease: true}}, nil
		case "recent":
			return domain.MultipartUploadSessionCleanupFacts{}, nil
		case "broken":
			return domain.MultipartUploadSessionCleanupFacts{}, lookupFailure
		default:
			return domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true}, nil
		}
	})
	first, err := scanner.ScanPages(t.Context(), target, "", 2, 2, time.Minute)
	if err != nil || first.PagesScanned != 2 || first.Scanned != 4 || first.NextUploadID != "broken" ||
		first.SkippedBound != 1 || first.SkippedCreation != 1 || first.SkippedWithinGrace != 1 || len(first.Candidates) != 0 ||
		!reflect.DeepEqual(first.Failures, []MultipartUploadSessionScanFailure{{UploadID: "broken", Err: lookupFailure}}) ||
		!reflect.DeepEqual(markers, []string{"", "creating"}) {
		t.Fatal("前两页汇总或提前停止不符", first, markers, err)
	}
	second, err := scanner.ScanPages(t.Context(), target, first.NextUploadID, 2, 2, time.Minute)
	if err != nil || second.PagesScanned != 1 || second.Scanned != 2 || second.NextUploadID != "" ||
		!reflect.DeepEqual(second.Candidates, sessions[4:]) || !reflect.DeepEqual(markers, []string{"", "creating", "broken"}) ||
		!reflect.DeepEqual(checked, []string{"bound", "creating", "recent", "broken", "candidate", "last"}) {
		t.Fatal("续扫未从断点读到末页", second, markers, checked, err)
	}
}

func TestMultipartUploadSessionCandidateScannerScanPagesPreservesInterruptedCursor(t *testing.T) {
	target, sessions := sessionCandidateFixture("first", "cursor", "partial", "broken", "before")
	storageFailure := errors.New("fixture page lookup failure")
	lookupFailure := errors.New("fixture independent failure")
	for _, fixture := range []struct {
		name string
		want error
	}{
		{"invalid budget", nil},
		{"storage failure", storageFailure},
		{"cursor cycle", domain.ErrUploadStorageUnavailable},
		{"canceled during second page", context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var markers []string
			scanner := NewMultipartUploadSessionCandidateScanner("local_minio", "fixture-bucket", func(_ context.Context, _, marker string, _ int) (*domain.MultipartUploadSessionPage, error) {
				markers = append(markers, marker)
				switch marker {
				case "before":
					return &domain.MultipartUploadSessionPage{Sessions: sessions[:2], NextUploadID: "cursor"}, nil
				case "cursor":
					switch fixture.want {
					case storageFailure:
						return nil, storageFailure
					case domain.ErrUploadStorageUnavailable:
						return &domain.MultipartUploadSessionPage{Sessions: sessions[4:], NextUploadID: "before"}, nil
					default:
						return &domain.MultipartUploadSessionPage{Sessions: sessions[2:4], NextUploadID: "broken"}, nil
					}
				default:
					t.Fatalf("中断后又发起读取 %q", marker)
					return nil, nil
				}
			}, func(_ context.Context, _, _ string, _ domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, _ time.Duration) (domain.MultipartUploadSessionCleanupFacts, error) {
				if session.UploadID == "broken" {
					cancel()
					return domain.MultipartUploadSessionCleanupFacts{}, errors.Join(ctx.Err(), lookupFailure)
				}
				return domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true}, nil
			})
			if fixture.want == nil {
				got, err := scanner.ScanPages(ctx, target, "before", 2, 0, time.Minute)
				if err == nil || len(markers) != 0 || got.PagesScanned != 0 || got.NextUploadID != "before" {
					t.Fatal("非正数页数未在读取前拒绝", got, markers, err)
				}
				return
			}
			got, err := scanner.ScanPages(ctx, target, "before", 2, 3, time.Minute)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(markers, []string{"before", "cursor"}) ||
				got.PagesScanned != 1 || got.Scanned != 2 || got.NextUploadID != "cursor" ||
				!reflect.DeepEqual(got.Candidates, sessions[:2]) || len(got.Failures) != 0 {
				t.Fatal("中断页的结果混入汇总或游标被推进", got, markers, err)
			}
			if fixture.want == context.Canceled {
				if len(got.InterruptedFailures) != 1 || got.InterruptedFailures[0].UploadID != "broken" ||
					!errors.Is(got.InterruptedFailures[0].Err, lookupFailure) {
					t.Fatal("取消前的独立错误未单独保留", got)
				}
			}
		})
	}
}
