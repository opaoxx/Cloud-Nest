package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadSessionCandidateRecheckerObservesCurrentProtection(t *testing.T) {
	target, sessions := sessionCandidateFixture("original-session")
	original := sessions[0]
	storageFailure := errors.New("fixture storage failure")
	databaseFailure := errors.New("fixture database failure")
	for _, fixture := range []struct {
		name         string
		exists       bool
		facts        domain.MultipartUploadSessionCleanupFacts
		status       domain.UploadStatus
		storageErr   error
		databaseErr  error
		cancelAt     string
		wantErr      error
		wantEligible bool
		wantLock     bool
	}{
		{name: "expired unprotected", exists: true, status: domain.UploadStatusUploading,
			facts: domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true}, wantEligible: true, wantLock: true},
		{name: "now bound", exists: true, status: domain.UploadStatusUploading, facts: domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true,
			Protection: domain.MultipartUploadSessionProtection{Bound: true}}, wantLock: true},
		{name: "creating with lease", exists: true, status: domain.UploadStatusCreating, facts: domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true,
			Protection: domain.MultipartUploadSessionProtection{ActiveCreationLease: true}}, wantLock: true},
		{name: "creating without lease", exists: true, status: domain.UploadStatusCreating,
			facts: domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true}, wantLock: true},
		{name: "within grace", exists: true, status: domain.UploadStatusUploading, wantLock: true},
		{name: "session absent"},
		{name: "storage error", storageErr: storageFailure, wantErr: storageFailure},
		{name: "database error", exists: true, databaseErr: databaseFailure, wantErr: databaseFailure, wantLock: true},
		{name: "cancel during storage", exists: true, cancelAt: "storage", wantErr: context.Canceled},
		{name: "cancel during database", exists: true, cancelAt: "database", wantErr: context.Canceled, wantLock: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls []string
			lockedResult := domain.MultipartUploadSessionLockedCheck{TaskStatus: fixture.status, Facts: fixture.facts}
			rechecker := NewMultipartUploadSessionCandidateRechecker("local_minio", "fixture-bucket", func(callContext context.Context, key, uploadID string) (bool, error) {
				calls = append(calls, "storage")
				if callContext != ctx || key != target.ObjectKey || uploadID != original.UploadID {
					t.Fatal("没有查询原会话")
				}
				if fixture.cancelAt == "storage" {
					cancel()
				}
				return fixture.exists, fixture.storageErr
			}, func(callContext context.Context, provider, bucket string, gotTarget domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, grace time.Duration) (domain.MultipartUploadSessionLockedCheck, error) {
				calls = append(calls, "database")
				if callContext != ctx || provider != "local_minio" || bucket != "fixture-bucket" || gotTarget != target || session != original || grace != time.Minute {
					t.Fatal("保护复核使用了不同的原对象或宽限期")
				}
				if fixture.cancelAt == "database" {
					cancel()
				}
				return lockedResult, fixture.databaseErr
			})
			wantCalls := []string{"storage"}
			if fixture.wantLock {
				wantCalls = append(wantCalls, "database")
			}
			want := MultipartUploadSessionCandidateCheck{}
			if fixture.exists && fixture.wantErr == nil {
				want = MultipartUploadSessionCandidateCheck{Exists: true, Database: lockedResult}
			}
			got, err := rechecker.Recheck(ctx, target, original, time.Minute)
			if !errors.Is(err, fixture.wantErr) || got != want || got.Eligible() != fixture.wantEligible || !reflect.DeepEqual(calls, wantCalls) {
				t.Fatal("当前状态或调用顺序不符", got, calls, err)
			}
		})
	}
}

func TestMultipartUploadSessionCandidateRecheckerRejectsInvalidScope(t *testing.T) {
	target, sessions := sessionCandidateFixture("original-session")
	original := sessions[0]
	rechecker := NewMultipartUploadSessionCandidateRechecker("local_minio", "fixture-bucket", func(context.Context, string, string) (bool, error) {
		t.Fatal("无效候选触发了存储查询")
		return false, nil
	}, func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionLockedCheck, error) {
		t.Fatal("无效候选触发了数据库查询")
		return domain.MultipartUploadSessionLockedCheck{}, nil
	})
	for _, fixture := range []struct {
		name string
		edit func(*domain.MultipartUploadSessionScanTarget, *domain.MultipartUploadSession)
		want error
	}{
		{"invalid task", func(target *domain.MultipartUploadSessionScanTarget, _ *domain.MultipartUploadSession) {
			target.TaskID = 0
		}, domain.ErrInvalidUploadTaskData},
		{"invalid upload ID", func(_ *domain.MultipartUploadSessionScanTarget, session *domain.MultipartUploadSession) {
			session.UploadID = ""
		}, domain.ErrInvalidUploadTaskData},
		{"other owner", func(target *domain.MultipartUploadSessionScanTarget, _ *domain.MultipartUploadSession) {
			target.UserID++
		}, domain.ErrInvalidStoredObjectData},
		{"other key", func(_ *domain.MultipartUploadSessionScanTarget, session *domain.MultipartUploadSession) {
			session.ObjectKey = "other"
		}, domain.ErrInvalidStoredObjectData},
		{"missing timestamp", func(_ *domain.MultipartUploadSessionScanTarget, session *domain.MultipartUploadSession) {
			session.InitiatedAt = time.Time{}
		}, domain.ErrInvalidStoredObjectData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			changedTarget, changedSession := target, original
			fixture.edit(&changedTarget, &changedSession)
			check, err := rechecker.Recheck(t.Context(), changedTarget, changedSession, time.Minute)
			if !errors.Is(err, fixture.want) || check.Eligible() {
				t.Fatal("无效范围未拒绝", check, err)
			}
		})
	}
}
