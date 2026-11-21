package service

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestRunMultipartUploadInitializationLoopWaitsAfterReporting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const interval = 5 * time.Second
		scanError := errors.New("fixture scan error")
		candidateError := errors.New("fixture candidate error")
		result := MultipartUploadInitializationBatchResult{
			Scanned: 3, Bound: 1, RetriesScheduled: 1,
			CandidateErrors: []MultipartUploadInitializationCandidateError{{TaskID: 19, Err: candidateError}},
		}
		started := time.Now()
		var starts []time.Duration
		reports := 0

		err := RunMultipartUploadInitializationLoop(ctx, interval, func(roundContext context.Context) (MultipartUploadInitializationBatchResult, error) {
			if roundContext != ctx {
				t.Fatal("未向单轮处理传递上下文")
			}
			starts = append(starts, time.Since(started))
			if len(starts) == 1 {
				time.Sleep(7 * time.Second)
				return MultipartUploadInitializationBatchResult{}, scanError
			}
			return result, nil
		}, func(summary MultipartUploadInitializationBatchResult, err error) {
			reports++
			if reports == 1 {
				if !errors.Is(err, scanError) || summary.Scanned != 0 {
					t.Fatal("首轮扫描错误未完整汇报", summary, err)
				}
				time.Sleep(time.Second)
				return
			}
			if err != nil || !reflect.DeepEqual(summary, result) {
				t.Fatal("第二轮绑定、重试或逐项错误未完整汇报", summary, err)
			}
			cancel()
		})
		// 首轮立即开始。处理耗时 7 秒、汇报耗时 1 秒，再等待完整的 5 秒。
		if !errors.Is(err, context.Canceled) || reports != 2 || !slices.Equal(starts, []time.Duration{0, 13 * time.Second}) {
			t.Fatal("处理、汇报、等待或退出顺序不符", starts, reports, err)
		}
	})
}

func TestRunMultipartUploadInitializationLoopStopsWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := time.Now()
		rounds, reports := 0, 0
		finished := make(chan error, 1)
		go func() {
			finished <- RunMultipartUploadInitializationLoop(ctx, 5*time.Second, func(context.Context) (MultipartUploadInitializationBatchResult, error) {
				rounds++
				return MultipartUploadInitializationBatchResult{}, nil
			}, func(MultipartUploadInitializationBatchResult, error) {
				reports++
			})
		}()
		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) || rounds != 1 || reports != 1 || time.Since(started) != time.Second {
			t.Fatal("循环未在轮间等待时立即退出", rounds, reports, time.Since(started), err)
		}
	})
}

func TestRunMultipartUploadInitializationLoopReportsActiveRoundOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		reports := 0
		err := RunMultipartUploadInitializationLoop(ctx, 5*time.Second, func(roundContext context.Context) (MultipartUploadInitializationBatchResult, error) {
			<-roundContext.Done()
			return MultipartUploadInitializationBatchResult{Scanned: 2, Bound: 1}, roundContext.Err()
		}, func(summary MultipartUploadInitializationBatchResult, err error) {
			reports++
			if !errors.Is(err, context.DeadlineExceeded) || summary.Scanned != 2 || summary.Bound != 1 {
				t.Fatal("在途轮次到期后未汇报已完成的结果", summary, err)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 {
			t.Fatal("循环未在汇报到期结果后退出", reports, err)
		}
	})
}
