package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadMergePreflightCheckMissingSession(t *testing.T) {
	const key, session = "users/7/objects/merge +%.bin", "Opaque+A/= "
	const version = "Version+A/="
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	missingObject, missingSession := domain.ErrUploadObjectMissing, domain.ErrUploadPartSessionMissing
	completionFailure := errors.Join(domain.ErrMultipartUploadCompletionUnknown, missingSession)
	flow := []string{"target", "head", "target", "list", "target", "head", "target"}
	for _, fixture := range []struct {
		name, cancelAt                           string
		failure, firstHead, listError, finalHead error
		targetChangedAt                          int
		leaseError                               error
		wantMissing                              bool
		want                                     error
		wantCalls                                []string
	}{
		{name: "recovers object already completed", failure: completionFailure, wantCalls: flow[:3]},
		{name: "recovers object appearing after session check", firstHead: missingObject, listError: missingSession, wantCalls: flow},
		{name: "observes both missing", firstHead: missingObject, listError: missingSession, finalHead: missingObject, wantMissing: true, wantCalls: flow},
		{name: "generic unknown completion has no missing signal", failure: domain.ErrMultipartUploadCompletionUnknown, want: domain.ErrUploadTaskNotReady},
		{name: "session still readable", firstHead: missingObject, want: domain.ErrUploadTaskNotReady, wantCalls: flow[:5]},
		{name: "first HEAD failed", firstHead: domain.ErrUploadStorageUnavailable, want: domain.ErrUploadStorageUnavailable, wantCalls: flow[:3]},
		{name: "session read failed", firstHead: missingObject, listError: context.DeadlineExceeded,
			want: domain.ErrUploadStorageUnavailable, wantCalls: flow[:4]},
		{name: "final HEAD failed", firstHead: missingObject, listError: missingSession, finalHead: domain.ErrUploadStorageUnavailable,
			want: domain.ErrUploadStorageUnavailable, wantCalls: flow},
		{name: "object size differs", firstHead: missingObject, listError: missingSession, finalHead: domain.ErrUploadBodySizeMismatch,
			want: domain.ErrUploadBodySizeMismatch, wantCalls: flow},
		{name: "lease already expired", targetChangedAt: 1, leaseError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: flow[:1]},
		{name: "lease lost during ListParts", firstHead: missingObject, listError: missingSession, targetChangedAt: 3, leaseError: domain.ErrUploadTaskBusy,
			want: domain.ErrUploadTaskBusy, wantCalls: flow[:5]},
		{name: "lease lost during final HEAD", firstHead: missingObject, listError: missingSession, finalHead: missingObject,
			targetChangedAt: 4, leaseError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: flow},
		{name: "target changes during final HEAD", firstHead: missingObject, listError: missingSession, finalHead: missingObject,
			targetChangedAt: 4, want: domain.ErrUploadTaskNotReady, wantCalls: flow},
		{name: "ListParts canceled", firstHead: missingObject, listError: missingSession, cancelAt: "list",
			want: context.Canceled, wantCalls: flow[:4]},
		{name: "final HEAD canceled", firstHead: missingObject, listError: missingSession, finalHead: missingObject, cancelAt: "final head",
			want: context.Canceled, wantCalls: flow[:6]},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 固定合成任务的租约、对象键和原会话。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.Status, claimed.LeaseOwner, claimed.LeaseUntil = domain.UploadStatusCompleting, new("merge-worker"), new(now.Add(time.Minute))
			claimed.StorageUploadID = new(session)
			original := copyUploadTask(claimed)
			target := UploadPartSigningTarget{TaskID: claimed.ID, UserID: claimed.UserID, StagedObjectID: claimed.StagedObjectID,
				LeaseVersion: claimed.LeaseVersion, ObjectKey: key, UploadID: session, FileSizeBytes: claimed.FileSize,
				PartSizeBytes: *claimed.PartSize, PartCount: *claimed.PartCount, ExpiresAt: claimed.ExpiresAt}

			// 2. 记录读取顺序，在指定位置模拟存储或租约变化。
			var calls []string
			targetReads, headReads := 0, 0
			getTarget := func(callContext context.Context, lease *domain.UploadTask) (*UploadPartSigningTarget, error) {
				calls = append(calls, "target")
				if callContext != ctx || lease != claimed {
					t.Fatal("复核目标未使用原租约")
				}
				targetReads++
				current := target
				if fixture.targetChangedAt == targetReads {
					current.ObjectKey += "-different"
					return &current, fixture.leaseError
				}
				return &current, nil
			}
			recovery := NewUploadMergeRecoveryService(getTarget, func(callContext context.Context, objectKey string) (*domain.UploadObjectResult, error) {
				calls = append(calls, "head")
				if callContext != ctx || objectKey != key {
					t.Fatal("HEAD 没有使用原对象键")
				}
				headReads++
				headError := fixture.firstHead
				if headReads == 2 {
					headError = fixture.finalHead
					if fixture.cancelAt == "final head" {
						cancel()
					}
				}
				if headError != nil {
					return nil, headError
				}
				return &domain.UploadObjectResult{SizeBytes: target.FileSizeBytes, StorageETag: new("object-etag"), StorageVersionID: new(version)}, nil
			})
			receipts := NewUploadMergeReceiptService(getTarget,
				func(context.Context, *domain.UploadTask) ([]domain.UploadPartReceipt, error) {
					t.Fatal("会话缺失复核不依赖旧冻结回执")
					return nil, nil
				},
				func(callContext context.Context, objectKey, uploadID string) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "list")
					if callContext != ctx || objectKey != key || uploadID != session {
						t.Fatal("ListParts 没有使用原对象键和原会话")
					}
					if fixture.cancelAt == "list" {
						cancel()
					}
					return nil, fixture.listError
				})
			failure := fixture.failure
			if failure == nil {
				failure = missingSession
			}

			// 3. 核对返回结果及调用顺序。拒绝结果不能携带缺失观察。
			result, err := NewUploadMergePreflightService(recovery, receipts).CheckMissingSession(ctx, claimed, failure)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(calls, fixture.wantCalls) || !reflect.DeepEqual(claimed, original) {
				t.Fatalf("复核结果或顺序不符，得到 %v、%v，预期 %v、%v", err, calls, fixture.want, fixture.wantCalls)
			}
			switch {
			case fixture.want != nil:
				if result != nil {
					t.Fatal("复核失败返回了结果", result)
				}
			case fixture.wantMissing:
				*claimed.StorageUploadID = "changed"
				if result == nil || result.Recovered != nil || result.Missing == nil ||
					result.Missing.ObjectKey != key || !reflect.DeepEqual(&result.Missing.Task, original) {
					t.Fatal("缺失观察没有保留独立的原会话和租约", result)
				}
			default:
				if result == nil || result.Missing != nil || result.Recovered == nil || result.Recovered.Target != target ||
					result.Recovered.Object.StorageVersionID == nil || *result.Recovered.Object.StorageVersionID != version {
					t.Fatal("对象恢复结果未携带原目标和确认版本", result)
				}
			}
		})
	}
}
