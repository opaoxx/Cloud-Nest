package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRunPurgeRootLoopReportsAndWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 让首轮耗时超过等待间隔，并在汇报时再经过一秒。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const interval = 5 * time.Second
		scanFailure := errors.New("fixture purge scan failure")
		startedAt := time.Now()
		var starts []time.Duration
		reports := 0
		err := RunPurgeRootLoop(ctx, interval, func(roundContext context.Context) (PurgeRootBatchResult, error) {
			if roundContext != ctx {
				t.Fatal("单轮清理没有收到循环上下文")
			}
			starts = append(starts, time.Since(startedAt))
			if len(starts) == 1 {
				time.Sleep(7 * time.Second)
				return PurgeRootBatchResult{}, scanFailure
			}
			return PurgeRootBatchResult{Scanned: 2, CompletedLeaves: 3, StaleRoots: 1}, nil
		}, func(summary PurgeRootBatchResult, roundError error) {
			reports++
			if reports == 1 {
				if !errors.Is(roundError, scanFailure) || summary.Scanned != 0 {
					t.Fatal("首轮扫描错误未交给回调", summary, roundError)
				}
				time.Sleep(time.Second)
				return
			}
			if roundError != nil || summary.Scanned != 2 || summary.CompletedLeaves != 3 || summary.StaleRoots != 1 ||
				len(summary.CandidateErrors) != 0 {
				t.Fatal("第二轮汇总不符", summary, roundError)
			}
			cancel()
		})

		// 2. 首轮立即启动，第二轮从上次汇报完成起再等满一个间隔。
		if !errors.Is(err, context.Canceled) || reports != 2 || !slices.Equal(starts, []time.Duration{0, 13 * time.Second}) {
			t.Fatal("首轮时间、轮间等待或取消退出不符", starts, reports, err)
		}
	})
}

func TestRunPurgeRootLoopStopsWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rounds := 0
		startedAt := time.Now()
		finished := make(chan error, 1)
		go func() {
			finished <- RunPurgeRootLoop(ctx, 5*time.Second, func(context.Context) (PurgeRootBatchResult, error) {
				rounds++
				return PurgeRootBatchResult{}, nil
			}, func(PurgeRootBatchResult, error) {})
		}()

		// 1. 等首轮进入轮间等待，在间隔结束前取消。
		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		err := <-finished
		if !errors.Is(err, context.Canceled) || rounds != 1 || time.Since(startedAt) != time.Second {
			t.Fatal("等待中的循环未及时退出", rounds, time.Since(startedAt), err)
		}
	})
}

func TestRunPurgeRootLoopReportsPartialProgressOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首个根项结算成功，第二个等到上下文到期。
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		candidates := []domain.PurgeRootCandidate{{UserID: 1, EntryID: 15}, {UserID: 2, EntryID: 16}}
		batch := NewPurgeRootBatch(func(context.Context, int) ([]domain.PurgeRootCandidate, error) {
			return candidates, nil
		}, func(roundContext context.Context, candidate domain.PurgeRootCandidate, _ int) (int, error) {
			if candidate == candidates[0] {
				return 1, nil
			}
			<-roundContext.Done()
			return 0, roundContext.Err()
		})

		// 2. 退出前向回调交付本轮已完成的数量和取消原因。
		reports := 0
		err := RunPurgeRootLoop(ctx, 5*time.Second, func(roundContext context.Context) (PurgeRootBatchResult, error) {
			return batch.RunOnce(roundContext, 2, 1)
		}, func(summary PurgeRootBatchResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.DeadlineExceeded) || summary.Scanned != 2 || summary.CompletedLeaves != 1 ||
				len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].RootID != 16 ||
				!errors.Is(summary.CandidateErrors[0].Err, context.DeadlineExceeded) {
				t.Fatal("执行中到期丢失了本轮汇总", summary, roundError)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 {
			t.Fatal("循环未在汇报后退出", reports, err)
		}
	})
}
