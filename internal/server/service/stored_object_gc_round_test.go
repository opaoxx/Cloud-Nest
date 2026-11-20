package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestStoredObjectGCRoundRejectsInvalidInputs(t *testing.T) {
	round := NewStoredObjectGCRound(func(context.Context, int) (StoredObjectDeletionRecoveryResult, error) {
		t.Fatal("无效输入进入了删除恢复批次")
		return StoredObjectDeletionRecoveryResult{}, nil
	}, func(context.Context, int) (StoredObjectGCBatchResult, error) {
		t.Fatal("无效输入进入了到期处理批次")
		return StoredObjectGCBatchResult{}, nil
	})
	for _, limits := range [][2]int{{0, 1}, {1, 0}, {domain.MaxStoredObjectGCCandidates + 1, 1}, {1, domain.MaxStoredObjectGCCandidates + 1}} {
		if _, err := round.RunOnce(t.Context(), limits[0], limits[1]); err == nil {
			t.Fatal("未拒绝无效上限", limits)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := round.RunOnce(ctx, 1, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("未返回上下文取消原因", err)
	}
}

func TestStoredObjectGCRoundRunsRecoveryBeforeDue(t *testing.T) {
	recoveryFailure := errors.New("恢复扫描失败")
	dueFailure := errors.New("到期扫描失败")
	recovery := StoredObjectDeletionRecoveryResult{
		Scanned: 2, Completed: 1,
		CandidateErrors: []StoredObjectDeletionCandidateError{{ObjectID: 33, OwnerUserID: 7, Err: domain.ErrStoredObjectDeleteUnavailable}},
	}
	due := StoredObjectGCBatchResult{Scanned: 2, Completed: 1, StaleCandidates: 1}
	for _, fixture := range []struct {
		name                string
		recoveryError       error
		dueError            error
		cancelAfterRecovery bool
		wantError           error
		wantDue             bool
	}{
		{name: "有单项恢复失败时仍处理到期项", wantDue: true},
		{name: "恢复批次失败后停止", recoveryError: recoveryFailure, wantError: recoveryFailure},
		{name: "两段之间取消后停止", cancelAfterRecovery: true, wantError: context.Canceled},
		{name: "到期批次失败时保留两段进度", dueError: dueFailure, wantError: dueFailure, wantDue: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			const recoveryLimit, dueLimit = domain.MaxStoredObjectGCCandidates, 2
			var calls []string
			round := NewStoredObjectGCRound(func(callContext context.Context, limit int) (StoredObjectDeletionRecoveryResult, error) {
				calls = append(calls, "恢复")
				if callContext != ctx || limit != recoveryLimit {
					t.Fatal("恢复批次未收到原上下文和对应上限", limit)
				}
				if fixture.cancelAfterRecovery {
					cancel()
				}
				return recovery, fixture.recoveryError
			}, func(callContext context.Context, limit int) (StoredObjectGCBatchResult, error) {
				calls = append(calls, "到期")
				if callContext != ctx || limit != dueLimit {
					t.Fatal("到期批次未收到原上下文和对应上限", limit)
				}
				return due, fixture.dueError
			})

			result, err := round.RunOnce(ctx, recoveryLimit, dueLimit)
			wantResult := StoredObjectGCRoundResult{Recovery: recovery}
			wantCalls := []string{"恢复"}
			if fixture.wantDue {
				wantResult.Due = due
				wantCalls = append(wantCalls, "到期")
			}
			if !errors.Is(err, fixture.wantError) || !slices.Equal(calls, wantCalls) || !reflect.DeepEqual(result, wantResult) {
				t.Fatal("执行顺序、分段结果或错误不符", calls, result, err)
			}
		})
	}
}
