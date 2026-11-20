package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestStoredObjectGCCandidateProcessor(t *testing.T) {
	// 1. 准备同一个对象在扫描、领取、完成时的三份记录。
	gcAfter := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	unreferencedAt := gcAfter.Add(-7 * 24 * time.Hour)
	candidate := domain.StoredObjectGCCandidate{
		ObjectID: 33, OwnerUserID: 7, Status: domain.StoredObjectStatusReady,
		UnreferencedAt: unreferencedAt, GCAfter: gcAfter,
	}
	deleting := domain.StoredObject{
		ID: candidate.ObjectID, OwnerUserID: candidate.OwnerUserID, Status: domain.StoredObjectStatusDeleting,
		UnreferencedAt: &unreferencedAt, GCAfter: &gcAfter,
	}
	deleted := deleting
	deleted.Status = domain.StoredObjectStatusDeleted
	wrongOwner := deleting
	wrongOwner.OwnerUserID++
	notClaimed := deleting
	notClaimed.Status = candidate.Status
	changedTimer := deleting
	newDeadline := gcAfter.Add(time.Hour)
	changedTimer.GCAfter = &newDeadline
	deletionFailure := errors.New("存储不可用")

	// 2. 覆盖领取前、领取时和领取后的分支。
	for _, test := range []struct {
		name             string
		invalidCandidate bool
		cancelBefore     bool
		cancelAfterClaim bool
		claimed          *domain.StoredObject
		claimError       error
		deleteError      error
		wantError        error
		wantClaims       int
		wantDeletions    int
	}{
		{name: "成功", claimed: &deleting, wantClaims: 1, wantDeletions: 1},
		{name: "候选格式无效", invalidCandidate: true, wantError: domain.ErrInvalidStoredObjectData},
		{name: "领取前取消", cancelBefore: true, wantError: context.Canceled},
		{name: "候选已失效", claimError: domain.ErrStoredObjectGCCandidateStale, wantError: domain.ErrStoredObjectGCCandidateStale, wantClaims: 1},
		{name: "仍被引用", claimError: domain.ErrStoredObjectInUse, wantError: domain.ErrStoredObjectInUse, wantClaims: 1},
		{name: "缺少领取结果", wantError: domain.ErrInvalidStoredObjectData, wantClaims: 1},
		{name: "领取结果归属不符", claimed: &wrongOwner, wantError: domain.ErrInvalidStoredObjectData, wantClaims: 1},
		{name: "领取结果尚未进入删除中", claimed: &notClaimed, wantError: domain.ErrInvalidStoredObjectData, wantClaims: 1},
		{name: "领取结果到期时间改变", claimed: &changedTimer, wantError: domain.ErrInvalidStoredObjectData, wantClaims: 1},
		{name: "领取后取消", claimed: &deleting, cancelAfterClaim: true, wantError: context.Canceled, wantClaims: 1},
		{name: "删除失败", claimed: &deleting, deleteError: deletionFailure, wantError: deletionFailure, wantClaims: 1, wantDeletions: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			input := candidate
			if test.invalidCandidate {
				input.ObjectID = 0
			}
			if test.cancelBefore {
				cancel()
			}
			claimCalls, deleteCalls := 0, 0
			processor := NewStoredObjectGCCandidateProcessor(
				func(callContext context.Context, scanned domain.StoredObjectGCCandidate) (*domain.StoredObject, error) {
					claimCalls++
					if callContext != ctx || scanned != input {
						t.Fatal("领取应收到原上下文和扫描候选")
					}
					if test.cancelAfterClaim {
						cancel()
					}
					return test.claimed, test.claimError
				},
				func(callContext context.Context, claimed *domain.StoredObject) (*domain.StoredObject, error) {
					deleteCalls++
					if claimCalls != 1 || callContext != ctx || claimed != test.claimed {
						t.Fatal("删除应在领取后收到原上下文和已提交记录")
					}
					if test.deleteError != nil {
						return nil, test.deleteError
					}
					return &deleted, nil
				},
			)

			// 3. 检查执行顺序、调用次数及返回值。
			result, err := processor.Process(ctx, input)
			if !errors.Is(err, test.wantError) || claimCalls != test.wantClaims || deleteCalls != test.wantDeletions {
				t.Fatalf("领取 %d 次、删除 %d 次，返回错误 %v", claimCalls, deleteCalls, err)
			}
			if test.wantError == nil && result != &deleted || test.wantError != nil && result != nil {
				t.Fatalf("返回记录不符: %+v", result)
			}
		})
	}
}
