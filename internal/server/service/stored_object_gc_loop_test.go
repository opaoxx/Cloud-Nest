package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRunStoredObjectGCLoopWaitsAfterReportingAndRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮报告扫描失败，第二轮报告恢复和到期两段结果。
		const interval = time.Minute
		const stopAfter = interval + 4*time.Second
		ctx, cancel := context.WithTimeout(t.Context(), stopAfter)
		defer cancel()
		scanFailure := errors.New("对象恢复扫描失败")
		expected := StoredObjectGCRoundResult{
			Recovery: StoredObjectDeletionRecoveryResult{Scanned: 2, Completed: 1, CandidateErrors: []StoredObjectDeletionCandidateError{
				{ObjectID: 33, OwnerUserID: 7, Err: domain.ErrStoredObjectDeleteUnavailable},
			}},
			Due: StoredObjectGCBatchResult{Scanned: 2, Completed: 1, StaleCandidates: 1},
		}
		start := time.Now()
		var roundStarts []time.Duration
		reports := 0
		err := RunStoredObjectGCLoop(ctx, interval, func(roundContext context.Context) (StoredObjectGCRoundResult, error) {
			if roundContext != ctx || len(roundStarts) != reports {
				t.Fatal("单轮执行应收到原上下文，并等待上轮汇报结束")
			}
			roundStarts = append(roundStarts, time.Since(start))
			if len(roundStarts) == 1 {
				time.Sleep(2 * time.Second)
				return StoredObjectGCRoundResult{}, scanFailure
			}
			return expected, nil
		}, func(summary StoredObjectGCRoundResult, roundError error) {
			reports++
			if reports == 1 {
				if !errors.Is(roundError, scanFailure) || summary.Recovery.Scanned != 0 || summary.Due.Scanned != 0 {
					t.Fatal("首轮扫描错误未交付", summary, roundError)
				}
				time.Sleep(time.Second)
				return
			}
			if roundError != nil || !reflect.DeepEqual(summary, expected) {
				t.Fatal("下一轮的两段结果未完整交付", summary, roundError)
			}
		})

		// 2. 首轮立即开始，第二轮在执行、汇报及完整等待后开始。
		if !errors.Is(err, context.DeadlineExceeded) || reports != 2 || time.Since(start) != stopAfter ||
			!slices.Equal(roundStarts, []time.Duration{0, interval + 3*time.Second}) {
			t.Fatal("轮间时序、汇报次数或退出原因不符", roundStarts, reports, time.Since(start), err)
		}
	})
}

func TestRunStoredObjectGCLoopReportsPartialRoundOnStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 恢复已完成，到期阶段等到上下文取消，带回已提交进度。
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		round := NewStoredObjectGCRound(func(context.Context, int) (StoredObjectDeletionRecoveryResult, error) {
			return StoredObjectDeletionRecoveryResult{Scanned: 1, Completed: 1}, nil
		}, func(roundContext context.Context, _ int) (StoredObjectGCBatchResult, error) {
			<-roundContext.Done()
			return StoredObjectGCBatchResult{Scanned: 2, Completed: 1}, roundContext.Err()
		})
		runs, reports := 0, 0
		err := RunStoredObjectGCLoop(ctx, time.Minute, func(roundContext context.Context) (StoredObjectGCRoundResult, error) {
			runs++
			return round.RunOnce(roundContext, 1, 2)
		}, func(summary StoredObjectGCRoundResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.DeadlineExceeded) || summary.Recovery.Scanned != 1 ||
				summary.Recovery.Completed != 1 || summary.Due.Scanned != 2 || summary.Due.Completed != 1 {
				t.Fatal("退出前应交付已完成进度和取消原因", summary, roundError)
			}
		})

		// 2. 停止信号结束当前轮，循环完成汇报后退出。
		if !errors.Is(err, context.DeadlineExceeded) || runs != 1 || reports != 1 {
			t.Fatal("取消后执行或汇报次数不符", runs, reports, err)
		}
	})
}
