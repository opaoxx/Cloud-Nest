package service

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestRunUploadVerificationLoopContinuesAfterRoundError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const interval = 5 * time.Second
		scanFailure := errors.New("fixture scan failure")
		candidateFailure := errors.New("fixture candidate failure")
		start := time.Now()
		var starts []time.Duration
		reports := 0

		err := RunUploadVerificationLoop(ctx, interval, func(roundContext context.Context) (UploadVerificationBatchResult, error) {
			if roundContext != ctx {
				t.Fatal("未向单轮执行传递上下文")
			}
			starts = append(starts, time.Since(start))
			if len(starts) == 1 {
				time.Sleep(7 * time.Second)
				return UploadVerificationBatchResult{}, scanFailure
			}
			return UploadVerificationBatchResult{
				Scanned: 3, Published: 1, RetriesScheduled: 1,
				CandidateErrors: []UploadVerificationCandidateError{{TaskID: 19, Err: candidateFailure}},
			}, nil
		}, func(summary UploadVerificationBatchResult, roundError error) {
			reports++
			if reports == 1 {
				if !errors.Is(roundError, scanFailure) || summary.Scanned != 0 {
					t.Fatal("扫描错误未交给结果回调", summary, roundError)
				}
				time.Sleep(time.Second)
				return
			}
			if roundError != nil || summary.Scanned != 3 || summary.Published != 1 || summary.RetriesScheduled != 1 ||
				len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != 19 ||
				!errors.Is(summary.CandidateErrors[0].Err, candidateFailure) {
				t.Fatal("第二轮汇总未完整交付", summary, roundError)
			}
			cancel()
		})
		// 首轮立即开始。首次执行和汇报共用 8 秒，之后再等待完整间隔。
		if !errors.Is(err, context.Canceled) || reports != 2 || !slices.Equal(starts, []time.Duration{0, 13 * time.Second}) {
			t.Fatal("定时循环的执行或退出顺序不符", starts, reports, err)
		}
	})
}

func TestRunUploadVerificationLoopStopsWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rounds, reports := 0, 0
		finished := make(chan error, 1)
		start := time.Now()
		go func() {
			finished <- RunUploadVerificationLoop(ctx, 5*time.Second, func(context.Context) (UploadVerificationBatchResult, error) {
				rounds++
				return UploadVerificationBatchResult{}, nil
			}, func(UploadVerificationBatchResult, error) {
				reports++
			})
		}()

		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		err := <-finished
		if !errors.Is(err, context.Canceled) || rounds != 1 || reports != 1 || time.Since(start) != time.Second {
			t.Fatal("等待中的循环未及时退出", rounds, reports, time.Since(start), err)
		}
	})
}

func TestRunUploadVerificationLoopReportsActiveRoundOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		reports := 0
		err := RunUploadVerificationLoop(ctx, 5*time.Second, func(roundContext context.Context) (UploadVerificationBatchResult, error) {
			<-roundContext.Done()
			return UploadVerificationBatchResult{Scanned: 2, Published: 1}, roundContext.Err()
		}, func(summary UploadVerificationBatchResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.DeadlineExceeded) || summary.Scanned != 2 || summary.Published != 1 {
				t.Fatal("运行中到期丢失了本轮部分汇总", summary, roundError)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 {
			t.Fatal("运行中到期后的退出结果错误", reports, err)
		}
	})
}

func TestRunUploadVerificationLoopInputs(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, scenario := range []struct {
		name          string
		ctx           context.Context
		interval      time.Duration
		missingRun    bool
		missingReport bool
		wantError     error
	}{
		{name: "missing context", interval: time.Second},
		{name: "zero interval", ctx: t.Context()},
		{name: "negative interval", ctx: t.Context(), interval: -time.Second},
		{name: "missing round", ctx: t.Context(), interval: time.Second, missingRun: true},
		{name: "missing report", ctx: t.Context(), interval: time.Second, missingReport: true},
		{name: "already canceled", ctx: canceled, interval: time.Second, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runOnce := func(context.Context) (UploadVerificationBatchResult, error) {
				t.Fatal("参数无效或上下文已取消时启动了校验")
				return UploadVerificationBatchResult{}, nil
			}
			report := func(UploadVerificationBatchResult, error) {
				t.Fatal("参数无效或上下文已取消时调用了结果回调")
			}
			if scenario.missingRun {
				runOnce = nil
			}
			if scenario.missingReport {
				report = nil
			}
			if err := RunUploadVerificationLoop(scenario.ctx, scenario.interval, runOnce, report); err == nil ||
				(scenario.wantError != nil && !errors.Is(err, scenario.wantError)) {
				t.Fatal("定时循环未拒绝无效输入或保留取消原因", err)
			}
		})
	}
}
