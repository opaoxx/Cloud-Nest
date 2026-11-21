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

func TestUploadMergePreflightServiceCheck(t *testing.T) {
	const key, session = "users/7/objects/merge +%.bin", "Opaque+A/= "
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	fullFlow := []string{"target", "head", "target", "frozen", "list", "target"}
	missing := domain.ErrUploadObjectMissing
	for _, fixture := range []struct {
		name, success, cancelAt           string
		headError, frozenError, listError error
		firstTargetError, recheckError    error
		wrongSize, mismatch               bool
		changeKeyBeforeList, changeLease  bool
		want                              error
		wantCalls                         []string
	}{
		{name: "recover the existing version", success: "recovered", wantCalls: fullFlow[:3]},
		{name: "verified receipts after definite absence", headError: missing, success: "ready", wantCalls: fullFlow},
		{name: "target rejects expired lease", firstTargetError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow[:1]},
		{name: "HEAD unavailable", headError: domain.ErrUploadStorageUnavailable, want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow[:3]},
		{name: "HEAD times out", headError: context.DeadlineExceeded, want: context.DeadlineExceeded, wantCalls: fullFlow[:3]},
		{name: "existing object has wrong size", wrongSize: true, want: domain.ErrUploadBodySizeMismatch, wantCalls: fullFlow[:3]},
		{name: "HEAD absence under old lease", headError: missing, recheckError: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow[:3]},
		{name: "frozen receipt read rejects stale lease", headError: missing, frozenError: domain.ErrUploadTaskBusy,
			want: domain.ErrUploadTaskBusy, wantCalls: fullFlow[:4]},
		{name: "session disappears after HEAD", headError: missing, listError: domain.ErrUploadPartSessionMissing, want: domain.ErrUploadPartSessionMissing, wantCalls: fullFlow},
		{name: "complete list could not be read", headError: missing, listError: domain.ErrUploadStorageUnavailable,
			want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow[:5]},
		{name: "tail receipt mismatches", headError: missing, mismatch: true, want: domain.ErrUploadPartsMismatch, wantCalls: fullFlow},
		{name: "changed key does not replace original target", headError: missing, changeKeyBeforeList: true, want: domain.ErrUploadTaskNotReady, wantCalls: fullFlow},
		{name: "lease changes during ListParts", headError: missing, changeLease: true, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "HEAD canceled", headError: missing, cancelAt: "head", want: context.Canceled, wantCalls: fullFlow[:2]},
		{name: "list canceled", headError: missing, cancelAt: "list", want: context.Canceled, wantCalls: fullFlow[:5]},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 准备同一租约的原目标、对象元数据和两个分片。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.Status, claimed.LeaseOwner, claimed.LeaseUntil = domain.UploadStatusCompleting, new("merge-worker"), new(now.Add(time.Minute))
			claimed.StorageUploadID = new(session)
			original := copyUploadTask(claimed)
			target := UploadPartSigningTarget{TaskID: claimed.ID, UserID: claimed.UserID, StagedObjectID: claimed.StagedObjectID,
				LeaseVersion: claimed.LeaseVersion, ObjectKey: key, UploadID: session, FileSizeBytes: claimed.FileSize,
				PartSizeBytes: *claimed.PartSize, PartCount: *claimed.PartCount, ExpiresAt: claimed.ExpiresAt}
			metadata := domain.UploadObjectResult{SizeBytes: claimed.FileSize, StorageETag: new("opaque-etag"), StorageVersionID: new("Version+A/=")}
			if fixture.wrongSize {
				metadata.SizeBytes--
			}
			frozen := []domain.UploadPartReceipt{uploadPartReceiptFixture(1, *claimed.PartSize, "First"), uploadPartReceiptFixture(2, 1, "Tail")}
			observed := slices.Clone(frozen)
			if fixture.mismatch {
				observed[1].ETag = "Different"
			}

			// 2. 运行真实只读服务，用夹具模拟 HEAD 与 ListParts 之间的变化。
			var calls []string
			loaded := target
			getTarget := func(queryContext context.Context, lease *domain.UploadTask) (*UploadPartSigningTarget, error) {
				calls = append(calls, "target")
				if queryContext != ctx || lease != claimed {
					t.Fatal("目标复查未使用原租约")
				}
				switch len(calls) {
				case 1:
					return &loaded, fixture.firstTargetError
				case 3:
					return &loaded, fixture.recheckError
				default:
					return &loaded, nil
				}
			}
			recovery := NewUploadMergeRecoveryService(getTarget, func(queryContext context.Context, objectKey string) (*domain.UploadObjectResult, error) {
				calls = append(calls, "head")
				if queryContext != ctx || objectKey != key {
					t.Fatal("HEAD 未查询原键")
				}
				if fixture.cancelAt == "head" {
					cancel()
				}
				if fixture.headError != nil {
					return nil, fixture.headError
				}
				return &metadata, nil
			})
			receipts := NewUploadMergeReceiptService(getTarget,
				func(queryContext context.Context, lease *domain.UploadTask) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "frozen")
					if queryContext != ctx || lease != claimed {
						t.Fatal("冻结回执未使用同一租约")
					}
					if fixture.changeKeyBeforeList {
						loaded.ObjectKey = key + "-changed"
					}
					return frozen, fixture.frozenError
				},
				func(queryContext context.Context, objectKey, uploadID string) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "list")
					if queryContext != ctx || objectKey != key || uploadID != session {
						t.Fatal("ListParts 未沿用 HEAD 确认的原目标")
					}
					if fixture.changeLease {
						loaded.LeaseVersion++
					}
					if fixture.cancelAt == "list" {
						cancel()
					}
					return observed, fixture.listError
				})

			// 3. 核对结果只包含当前分支的依据，失败时不交付部分成功。
			result, err := NewUploadMergePreflightService(recovery, receipts).Check(ctx, claimed)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(calls, fixture.wantCalls) || !reflect.DeepEqual(claimed, original) {
				t.Fatalf("结果、调用顺序或原租约不符，得到 %v、%v，预期 %v、%v", err, calls, fixture.want, fixture.wantCalls)
			}
			if fixture.want != nil {
				if result != nil {
					t.Fatal("失败时仍交付可执行结果")
				}
				var mismatch *domain.UploadPartsMismatchError
				if fixture.mismatch && (!errors.As(err, &mismatch) || !slices.Equal(mismatch.PartNumbers, []uint32{2})) {
					t.Fatal("预检未保留失配的分片编号", err)
				}
				return
			}
			if result == nil {
				t.Fatal("成功缺少预检结果")
			}
			switch fixture.success {
			case "recovered":
				if result.Ready != nil || result.Recovered == nil || result.Recovered.Target != target || !reflect.DeepEqual(result.Recovered.Object, metadata) {
					t.Fatal("成功恢复应只交付确认的目标和对象元数据", result)
				}
			case "ready":
				if result.Recovered != nil || result.Ready == nil || result.Ready.Target != target || !slices.Equal(result.Ready.Parts, frozen) {
					t.Fatal("允许合并时应只交付原目标和完整回执", result)
				}
			default:
				t.Fatal("夹具未声明成功结果")
			}
		})
	}
}
