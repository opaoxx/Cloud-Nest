package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadCompletionReceiptServiceVerify(t *testing.T) {
	const key = "users/7/objects/verify-fixture"
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	fullFlow := []string{"target", "storage", "target"}
	targetAndStorage := fullFlow[:2]
	privateError := errors.New("private storage fixture")
	for _, test := range []struct {
		name                             string
		firstError, listError, lastError error
		changeFirst, changeLast          func(*UploadPartSigningTarget)
		cancelAt                         string
		badETag, missingStoragePart      bool
		want                             error
		wantCalls                        []string
	}{
		{name: "complete plan", wantCalls: fullFlow},
		{name: "first target rejects lost lease", firstError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow[:1]},
		{name: "first target contains another session", changeFirst: func(target *UploadPartSigningTarget) { target.UploadID = "unexpected" },
			want: domain.ErrInvalidUploadTaskData, wantCalls: fullFlow[:1]},
		{name: "first target contains another plan", changeFirst: func(target *UploadPartSigningTarget) { target.FileSizeBytes++ },
			want: domain.ErrInvalidUploadTaskData, wantCalls: fullFlow[:1]},
		{name: "old ETag", badETag: true, want: domain.ErrUploadPartsMismatch, wantCalls: fullFlow},
		{name: "missing part", missingStoragePart: true, want: domain.ErrUploadPartsMismatch, wantCalls: fullFlow},
		{name: "expired lease invalidates mismatch", badETag: true, lastError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "binding changes while listing", changeLast: func(target *UploadPartSigningTarget) { target.ObjectKey += "-new" },
			want: domain.ErrUploadTaskNotReady, wantCalls: fullFlow},
		{name: "same binding newer lease", changeLast: func(target *UploadPartSigningTarget) { target.LeaseVersion++ },
			want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "task deadline changed", changeLast: func(target *UploadPartSigningTarget) { target.ExpiresAt = target.ExpiresAt.Add(time.Minute) },
			want: domain.ErrUploadTaskNotReady, wantCalls: fullFlow},
		{name: "partial list with error", listError: privateError, want: domain.ErrUploadStorageUnavailable, wantCalls: targetAndStorage},
		{name: "storage timeout", listError: context.DeadlineExceeded, want: domain.ErrUploadStorageUnavailable, wantCalls: targetAndStorage},
		{name: "storage session missing", listError: domain.ErrUploadPartSessionMissing, want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow},
		{name: "session missing after lease loss", listError: domain.ErrUploadPartSessionMissing, lastError: domain.ErrUploadTaskBusy,
			want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "cancel before list", cancelAt: "target", want: context.Canceled, wantCalls: fullFlow[:1]},
		{name: "cancel while listing", cancelAt: "storage", want: context.Canceled, wantCalls: targetAndStorage},
		{name: "cancel during final read", cancelAt: "recheck", want: context.Canceled, wantCalls: fullFlow},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.LeaseOwner, claimed.LeaseUntil = new("fixture-verifier"), new(now.Add(time.Minute))
			before := copyUploadTask(claimed)
			target := &UploadPartSigningTarget{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: claimed.LeaseVersion,
				ObjectKey: key, UploadID: *claimed.StorageUploadID, FileSizeBytes: claimed.FileSize,
				PartSizeBytes: *claimed.PartSize, PartCount: *claimed.PartCount, ExpiresAt: claimed.ExpiresAt}
			observed := []domain.UploadPartReceipt{
				uploadPartReceiptFixture(1, *claimed.PartSize, "First"),
				uploadPartReceiptFixture(2, 1, "Tail"),
			}
			claims := []domain.UploadPartCompletionClaim{{PartNumber: 2, ETag: "Tail"}, {PartNumber: 1, ETag: "First"}}
			if test.badETag {
				claims[0].ETag = "OldTail"
			}
			if test.missingStoragePart {
				observed = observed[:1]
			}
			var calls []string
			verifier := NewUploadCompletionReceiptService(
				func(queryContext context.Context, currentActor domain.Actor, lease *domain.UploadTask) (*UploadPartSigningTarget, error) {
					calls = append(calls, "target")
					if queryContext != ctx || currentActor != actor || lease != claimed {
						t.Fatal("目标查询未使用原认证身份和租约")
					}
					if len(calls) == 1 {
						if test.changeFirst != nil {
							test.changeFirst(target)
						}
						if test.cancelAt == "target" {
							cancel()
						}
						return target, test.firstError
					}
					if test.changeLast != nil {
						test.changeLast(target)
					}
					if test.cancelAt == "recheck" {
						cancel()
					}
					return target, test.lastError
				},
				func(queryContext context.Context, objectKey, uploadID string) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "storage")
					if queryContext != ctx || objectKey != key || uploadID != *claimed.StorageUploadID {
						t.Fatal("分页读取未使用已复核的对象和会话")
					}
					if test.cancelAt == "storage" {
						cancel()
					}
					return observed, test.listError
				})
			result, err := verifier.Verify(ctx, actor, claimed, claims)
			if !errors.Is(err, test.want) || !reflect.DeepEqual(calls, test.wantCalls) || !reflect.DeepEqual(claimed, before) {
				t.Fatalf("核对结果、调用顺序或任务快照不符，得到 %v、%v，预期 %v、%v", err, calls, test.want, test.wantCalls)
			}
			if test.want != nil {
				if result != nil {
					t.Fatal("失败时返回了回执")
				}
				if errors.Is(test.want, domain.ErrUploadPartsMismatch) {
					var mismatch *domain.UploadPartsMismatchError
					if !errors.As(err, &mismatch) || !reflect.DeepEqual(mismatch.PartNumbers, []uint32{2}) {
						t.Fatal("失配错误未保留受影响编号", err)
					}
				}
				return
			}
			want := append([]domain.UploadPartReceipt(nil), observed...)
			clear(observed)
			if !reflect.DeepEqual(result, want) {
				t.Fatal("结果没有独立保留完整且有序的存储回执")
			}
		})
	}
}
