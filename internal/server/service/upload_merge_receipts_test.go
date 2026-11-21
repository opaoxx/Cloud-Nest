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

func TestUploadMergeReceiptServiceVerify(t *testing.T) {
	const key = "users/7/objects/merge +%.bin"
	const session = "Opaque+A/= "
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	fullFlow := []string{"target", "frozen", "list", "target"}
	privateError := errors.New("fixture storage failure")
	for _, fixture := range []struct {
		name                                  string
		mutateFrozen, mutateObserved          func([]domain.UploadPartReceipt) []domain.UploadPartReceipt
		changeFirst, changeLast               func(*UploadPartSigningTarget)
		firstErr, frozenErr, listErr, lastErr error
		cancelAt                              string
		want                                  error
		wantMismatch                          []uint32
		wantCalls                             []string
	}{
		{name: "matched frozen snapshot", wantCalls: fullFlow},
		{name: "first target rejects lease", firstErr: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow[:1]},
		{name: "first target has another session", changeFirst: func(target *UploadPartSigningTarget) { target.UploadID = "different" },
			want: domain.ErrInvalidUploadTaskData, wantCalls: fullFlow[:1]},
		{name: "frozen read rejected", frozenErr: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow[:2]},
		{name: "frozen snapshot is incomplete", mutateFrozen: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt { return parts[:1] },
			want: domain.ErrInvalidUploadTaskData, wantCalls: fullFlow[:2]},
		{name: "frozen tail size invalid", mutateFrozen: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt { parts[1].SizeBytes++; return parts },
			want: domain.ErrInvalidUploadTaskData, wantCalls: fullFlow[:2]},
		{name: "frozen SHA-256 invalid", mutateFrozen: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt {
			parts[1].ChecksumSHA256 = ""
			return parts
		},
			want: domain.ErrInvalidUploadTaskData, wantCalls: fullFlow[:2]},
		{name: "observed tail ETag changed", mutateObserved: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt {
			parts[1].ETag = "tail"
			return parts
		},
			want: domain.ErrUploadPartsMismatch, wantMismatch: []uint32{2}, wantCalls: fullFlow},
		{name: "observed tail SHA-256 changed with same ETag", mutateObserved: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt {
			parts[1].ChecksumSHA256 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
			return parts
		}, want: domain.ErrUploadPartsMismatch, wantMismatch: []uint32{2}, wantCalls: fullFlow},
		{name: "observed size changed", mutateObserved: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt { parts[0].SizeBytes++; return parts },
			want: domain.ErrUploadPartsMismatch, wantMismatch: []uint32{1}, wantCalls: fullFlow},
		{name: "observed part missing", mutateObserved: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt { return parts[1:] },
			want: domain.ErrUploadPartsMismatch, wantMismatch: []uint32{1}, wantCalls: fullFlow},
		{name: "empty but successful list", mutateObserved: func([]domain.UploadPartReceipt) []domain.UploadPartReceipt { return nil },
			want: domain.ErrUploadPartsMismatch, wantMismatch: []uint32{1, 2}, wantCalls: fullFlow},
		{name: "duplicate observed part", mutateObserved: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt {
			parts[1].PartNumber = 1
			return parts
		},
			want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow},
		{name: "observed part out of plan", mutateObserved: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt {
			parts[1].PartNumber = 3
			return parts
		},
			want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow},
		{name: "malformed observed receipt even with wrong size", mutateObserved: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt {
			parts[1].SizeBytes, parts[1].ETag = 2, "bad etag"
			return parts
		}, want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow},
		{name: "missing session preserves signal", listErr: errors.Join(privateError, domain.ErrUploadPartSessionMissing),
			want: domain.ErrUploadPartSessionMissing, wantCalls: fullFlow},
		{name: "missing session with stale lease", listErr: domain.ErrUploadPartSessionMissing, lastErr: domain.ErrUploadTaskBusy,
			want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "partial results with query failure", listErr: privateError, want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow[:3]},
		{name: "storage deadline", listErr: context.DeadlineExceeded, want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow[:3]},
		{name: "changed lease after matching", changeLast: func(target *UploadPartSigningTarget) { target.LeaseVersion++ },
			want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "changed key after matching", changeLast: func(target *UploadPartSigningTarget) { target.ObjectKey += "-new" },
			want: domain.ErrUploadTaskNotReady, wantCalls: fullFlow},
		{name: "stale lease supersedes mismatch", mutateObserved: func(parts []domain.UploadPartReceipt) []domain.UploadPartReceipt { return parts[:1] },
			lastErr: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "cancel while reading frozen", cancelAt: "frozen", want: context.Canceled, wantCalls: fullFlow[:2]},
		{name: "cancel while reading storage", cancelAt: "list", want: context.Canceled, wantCalls: fullFlow[:3]},
		{name: "cancel while rechecking target", cancelAt: "recheck", want: context.Canceled, wantCalls: fullFlow},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 独立构造冻结回执和存储清单，模拟一次后台租约。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.Status, claimed.LeaseOwner, claimed.LeaseUntil = domain.UploadStatusCompleting, new("merge-worker"), new(now.Add(time.Minute))
			claimed.StorageUploadID = new(session)
			originalClaim := copyUploadTask(claimed)
			target := UploadPartSigningTarget{TaskID: claimed.ID, UserID: claimed.UserID, StagedObjectID: claimed.StagedObjectID, LeaseVersion: claimed.LeaseVersion,
				ObjectKey: key, UploadID: session, FileSizeBytes: claimed.FileSize, PartSizeBytes: *claimed.PartSize, PartCount: *claimed.PartCount, ExpiresAt: claimed.ExpiresAt}
			expected := []domain.UploadPartReceipt{uploadPartReceiptFixture(1, *claimed.PartSize, "First"), uploadPartReceiptFixture(2, 1, "Tail")}
			frozen, observed := slices.Clone(expected), slices.Clone(expected)
			if fixture.mutateFrozen != nil {
				frozen = fixture.mutateFrozen(frozen)
			}
			if fixture.mutateObserved != nil {
				observed = fixture.mutateObserved(observed)
			}

			// 2. 验证目标、冻结回执和列表的调用顺序与原会话参数。
			var calls []string
			loaded := target
			verifier := NewUploadMergeReceiptService(
				func(queryContext context.Context, lease *domain.UploadTask) (*UploadPartSigningTarget, error) {
					calls = append(calls, "target")
					if queryContext != ctx || lease != claimed {
						t.Fatal("目标读取未使用当前租约")
					}
					if len(calls) == 1 {
						if fixture.changeFirst != nil {
							fixture.changeFirst(&loaded)
						}
						return &loaded, fixture.firstErr
					}
					if fixture.changeLast != nil {
						fixture.changeLast(&loaded)
					}
					if fixture.cancelAt == "recheck" {
						cancel()
					}
					return &loaded, fixture.lastErr
				},
				func(queryContext context.Context, lease *domain.UploadTask) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "frozen")
					if queryContext != ctx || lease != claimed {
						t.Fatal("冻结回执读取未使用当前租约")
					}
					if fixture.cancelAt == "frozen" {
						cancel()
					}
					return frozen, fixture.frozenErr
				},
				func(queryContext context.Context, objectKey, uploadID string) ([]domain.UploadPartReceipt, error) {
					calls = append(calls, "list")
					if queryContext != ctx || objectKey != key || uploadID != session {
						t.Fatal("存储查询未保留原对象键及会话 ID")
					}
					if fixture.cancelAt == "list" {
						cancel()
					}
					return observed, fixture.listErr
				})

			// 3. 核对错误类别、失配编号和成功结果的独立性。
			result, err := verifier.Verify(ctx, claimed)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(calls, fixture.wantCalls) || !reflect.DeepEqual(claimed, originalClaim) {
				t.Fatalf("核对结果、调用顺序或任务快照有误，得到 %v、%v，预期 %v、%v", err, calls, fixture.want, fixture.wantCalls)
			}
			if errors.Is(err, domain.ErrUploadPartsMismatch) != (len(fixture.wantMismatch) > 0) ||
				errors.Is(err, domain.ErrUploadPartSessionMissing) != (fixture.want == domain.ErrUploadPartSessionMissing) || errors.Is(err, privateError) {
				t.Fatal("失配、会话缺失与查询错误没有按预期区分", err)
			}
			if fixture.want != nil {
				if result != nil {
					t.Fatal("失败时交付了部分结果")
				}
				var mismatch *domain.UploadPartsMismatchError
				if len(fixture.wantMismatch) > 0 && (!errors.As(err, &mismatch) || !slices.Equal(mismatch.PartNumbers, fixture.wantMismatch)) {
					t.Fatal("失配编号不符", err)
				}
				return
			}
			clear(frozen)
			clear(observed)
			loaded.ObjectKey = "changed"
			if result == nil || result.Target != target || !slices.Equal(result.Parts, expected) {
				t.Fatal("结果未独立保留目标和有序的冻结回执", result)
			}
		})
	}
}

func TestUploadMergeReceiptMismatchLimit(t *testing.T) {
	const partSize = 16 << 20
	const partCount = domain.MaxUploadPartsMismatchNumbers + 5
	target := UploadPartSigningTarget{FileSizeBytes: (partCount-1)*partSize + 1, PartSizeBytes: partSize, PartCount: partCount}
	frozen := make([]domain.UploadPartReceipt, partCount)
	for index := range frozen {
		frozen[index] = uploadPartReceiptFixture(uint32(index+1), partSize, "Part")
	}
	frozen[partCount-1].SizeBytes = 1
	var mismatch *domain.UploadPartsMismatchError
	if err := compareUploadMergeReceipts(target, frozen, nil); !errors.As(err, &mismatch) || len(mismatch.PartNumbers) != domain.MaxUploadPartsMismatchNumbers {
		t.Fatal("失配编号超过约定上限或没有返回失配", err)
	}
	for index, actual := range mismatch.PartNumbers {
		if actual != uint32(index+1) {
			t.Fatal("失配编号未保留计划顺序", mismatch.PartNumbers)
		}
	}
}
