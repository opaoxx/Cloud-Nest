package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadMergePreflightCheckRepair(t *testing.T) {
	const key, session = "users/7/objects/merge +%.bin", "Opaque+A/= "
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	flow := []string{"target", "head", "target", "frozen", "list", "target", "head", "target"}
	missing := domain.ErrUploadObjectMissing
	for _, fixture := range []struct {
		name, listState         string
		failure, head, lastHead error
		frozenError, listError  error
		finalLeaseError         error
		cancelOnFinalHead       bool
		want                    error
		wantCalls               []string
	}{
		{name: "fresh mismatch", head: missing, lastHead: missing, wantCalls: flow},
		{name: "explicit rejection with fresh evidence", failure: domain.ErrMultipartUploadPartRejected, head: missing, lastHead: missing, wantCalls: flow},
		{name: "completion unknown is not a repair signal", failure: domain.ErrMultipartUploadCompletionUnknown, want: domain.ErrUploadTaskNotReady},
		{name: "already completed object", want: domain.ErrUploadObjectExists, wantCalls: flow[:3]},
		{name: "HEAD unclear", head: domain.ErrUploadStorageUnavailable, want: domain.ErrUploadStorageUnavailable, wantCalls: flow[:3]},
		{name: "frozen read failed", head: missing, frozenError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: flow[:4]},
		{name: "earlier mismatch not accepted as fresh evidence", head: missing, frozenError: &domain.UploadPartsMismatchError{PartNumbers: []uint32{1}},
			want: domain.ErrUploadPartsMismatch, wantCalls: flow[:4]},
		{name: "original session no longer exists", head: missing, listError: domain.ErrUploadPartSessionMissing,
			want: domain.ErrUploadPartSessionMissing, wantCalls: flow[:6]},
		{name: "partial list with error", head: missing, listError: domain.ErrUploadStorageUnavailable,
			want: domain.ErrUploadStorageUnavailable, wantCalls: flow[:5]},
		{name: "duplicate part is not evidence", head: missing, listState: "duplicate", want: domain.ErrUploadStorageUnavailable, wantCalls: flow[:6]},
		{name: "mismatch no longer present", head: missing, listState: "match", want: domain.ErrUploadTaskNotReady, wantCalls: flow[:6]},
		{name: "object appears after listing", head: missing, want: domain.ErrUploadObjectExists, wantCalls: flow},
		{name: "final HEAD fails", head: missing, lastHead: domain.ErrUploadStorageUnavailable, want: domain.ErrUploadStorageUnavailable, wantCalls: flow},
		{name: "latest lease was lost", head: missing, lastHead: missing, finalLeaseError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: flow},
		{name: "final HEAD canceled", head: missing, lastHead: missing, cancelOnFinalHead: true, want: context.Canceled, wantCalls: flow[:7]},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 准备原合并目标和固定两片回执。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.Status, claimed.LeaseOwner, claimed.LeaseUntil = domain.UploadStatusCompleting, new("merge-worker"), new(now.Add(time.Minute))
			claimed.StorageUploadID = new(session)
			original := copyUploadTask(claimed)
			target := UploadPartSigningTarget{TaskID: claimed.ID, UserID: claimed.UserID, StagedObjectID: claimed.StagedObjectID,
				LeaseVersion: claimed.LeaseVersion, ObjectKey: key, UploadID: session, FileSizeBytes: claimed.FileSize,
				PartSizeBytes: *claimed.PartSize, PartCount: *claimed.PartCount, ExpiresAt: claimed.ExpiresAt}
			frozen := []domain.UploadPartReceipt{uploadPartReceiptFixture(1, *claimed.PartSize, "First"), uploadPartReceiptFixture(2, 1, "Tail")}
			observed := slices.Clone(frozen)
			switch fixture.listState {
			case "match":
			case "duplicate":
				observed[1].PartNumber = 1
			default:
				observed[1].ETag = "Changed"
			}

			// 2. 通过真实服务顺序观察原键、原会话与第二次 HEAD。
			var calls []string
			headReads := 0
			getTarget := func(callContext context.Context, lease *domain.UploadTask) (*UploadPartSigningTarget, error) {
				calls = append(calls, "target")
				if callContext != ctx || lease != claimed {
					t.Fatal("目标复查没有使用原租约")
				}
				if headReads == 2 && fixture.finalLeaseError != nil {
					return nil, fixture.finalLeaseError
				}
				return &target, nil
			}
			recovery := NewUploadMergeRecoveryService(getTarget, func(callContext context.Context, objectKey string) (*domain.UploadObjectResult, error) {
				calls = append(calls, "head")
				if callContext != ctx || objectKey != key {
					t.Fatal("HEAD 没有使用原对象键")
				}
				headReads++
				headError := fixture.head
				if headReads == 2 {
					headError = fixture.lastHead
					if fixture.cancelOnFinalHead {
						cancel()
					}
				}
				if headError != nil {
					return nil, headError
				}
				return &domain.UploadObjectResult{SizeBytes: target.FileSizeBytes, StorageETag: new("object-etag")}, nil
			})
			receipts := NewUploadMergeReceiptService(getTarget,
				func(callContext context.Context, lease *domain.UploadTask) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "frozen")
					if callContext != ctx || lease != claimed {
						t.Fatal("冻结回执查询未复用原租约")
					}
					return frozen, fixture.frozenError
				},
				func(callContext context.Context, objectKey, uploadID string) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "list")
					if callContext != ctx || objectKey != key || uploadID != session {
						t.Fatal("ListParts 没有使用原对象键及会话")
					}
					return observed, fixture.listError
				})
			failure := fixture.failure
			if failure == nil {
				failure = &domain.UploadPartsMismatchError{PartNumbers: []uint32{1}}
			}

			// 3. 失配编号必须来自本轮完整清单，其他结果不交付修复依据。
			evidence, err := NewUploadMergePreflightService(recovery, receipts).CheckRepair(ctx, claimed, failure)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(calls, fixture.wantCalls) || !reflect.DeepEqual(claimed, original) {
				t.Fatalf("核对结果或调用顺序不符，得到 %v、%v，预期 %v、%v", err, calls, fixture.want, fixture.wantCalls)
			}
			if fixture.want != nil {
				if evidence != nil {
					t.Fatal("检查失败仍交付了修复依据")
				}
				return
			}
			if evidence == nil || evidence.Target != target || !slices.Equal(evidence.PartNumbers, []uint32{2}) {
				t.Fatal("修复依据不符，须保留原目标与本轮观察的失配编号", evidence)
			}
		})
	}
}
