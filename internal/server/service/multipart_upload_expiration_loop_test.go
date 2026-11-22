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

func TestRunMultipartUploadExpirationLoopWaitsAfterReporting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const interval = 30 * time.Second
		scanError := errors.New("fixture expiration scan failed")
		want := MultipartUploadExpirationBatchResult{Scanned: 4, Expired: 1, RetriesScheduled: 1, Skipped: 1,
			CandidateErrors: []UploadExpirationCandidateError{{TaskID: 20, Err: domain.ErrStoredObjectInUse}}}
		start := time.Now()
		var starts, reports []time.Duration

		// 1. 首轮耗时超过轮间隔，汇报本身也需要时间。
		err := RunMultipartUploadExpirationLoop(ctx, interval, func(roundContext context.Context) (MultipartUploadExpirationBatchResult, error) {
			if roundContext != ctx {
				t.Fatal("单轮未收到原上下文")
			}
			starts = append(starts, time.Since(start))
			if len(starts) == 1 {
				time.Sleep(35 * time.Second)
				return MultipartUploadExpirationBatchResult{}, scanError
			}
			return want, nil
		}, func(summary MultipartUploadExpirationBatchResult, err error) {
			reports = append(reports, time.Since(start))
			if len(reports) == 1 {
				if !errors.Is(err, scanError) || !reflect.DeepEqual(summary, MultipartUploadExpirationBatchResult{}) {
					t.Fatal("首轮扫描错误未随结果汇报", summary, err)
				}
				time.Sleep(2 * time.Second)
				return
			}
			if err != nil || !reflect.DeepEqual(summary, want) {
				t.Fatal("下一轮未汇报完整结果", summary, err)
			}
			cancel()
		})

		// 2. 第 37 秒结束汇报，再等 30 秒才开始第二轮。
		if !errors.Is(err, context.Canceled) || !slices.Equal(starts, []time.Duration{0, 67 * time.Second}) ||
			!slices.Equal(reports, []time.Duration{35 * time.Second, 67 * time.Second}) || time.Since(start) != 67*time.Second {
			t.Fatal("执行与汇报顺序或轮间等待不符", starts, reports, time.Since(start), err)
		}
	})
}

func TestRunMultipartUploadExpirationLoopStopsWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		rounds, reports := 0, 0
		finished := make(chan error, 1)
		go func() {
			finished <- RunMultipartUploadExpirationLoop(ctx, 30*time.Second,
				func(context.Context) (MultipartUploadExpirationBatchResult, error) {
					rounds++
					return MultipartUploadExpirationBatchResult{}, nil
				}, func(MultipartUploadExpirationBatchResult, error) { reports++ })
		}()

		// 1. 等首轮完成并开始等待，再于第 1 秒取消。
		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) || rounds != 1 || reports != 1 || time.Since(start) != time.Second {
			t.Fatal("取消未及时中止等待", rounds, reports, time.Since(start), err)
		}
	})
}

func TestRunMultipartUploadExpirationLoopReportsPartialBatchOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		const worker = "fixture-expiry-loop"
		failure := errors.New("fixture rollback failed")
		first, second := multipartExpirationBatchCandidates(time.Now().UTC())
		third, fourth := second, second
		third.TaskID, fourth.TaskID = 21, 22
		third.StagedObjectID, fourth.StagedObjectID = 25, 26
		candidates := []domain.UploadExpirationCandidate{first, second, third, fourth}
		var executed []uint64
		batch := NewMultipartUploadExpirationBatch(func(context.Context, int) ([]domain.UploadExpirationCandidate, error) {
			return candidates, nil
		}, func(_ context.Context, candidate domain.UploadExpirationCandidate, owner string) (*domain.UploadTask, error) {
			if candidate.TaskID == fourth.TaskID || owner != worker {
				t.Fatal("停止后仍领取候选或持有人不符")
			}
			return multipartExpirationBatchClaim(candidate, owner, time.Now().UTC()), nil
		}, func(roundContext context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
			executed = append(executed, claimed.ID)
			switch claimed.ID {
			case first.TaskID:
				return multipartExpirationBatchFinished(claimed), nil
			case second.TaskID:
				return nil, &MultipartUploadExpirationRetryScheduledError{Cause: domain.ErrUploadStorageUnavailable, NextAttemptAt: *claimed.LeaseUntil}
			default:
				<-roundContext.Done()
				return nil, errors.Join(roundContext.Err(), failure)
			}
		})

		// 1. 用正式单轮编排核对停止前的已提交结果和独立故障。
		reports := 0
		err := RunMultipartUploadExpirationLoop(ctx, 30*time.Second, func(roundContext context.Context) (MultipartUploadExpirationBatchResult, error) {
			return batch.RunOnce(roundContext, len(candidates), worker)
		}, func(summary MultipartUploadExpirationBatchResult, err error) {
			reports++
			if !errors.Is(err, context.DeadlineExceeded) || summary.Scanned != 4 || summary.Expired != 1 ||
				summary.RetriesScheduled != 1 || summary.Skipped != 0 || len(summary.CandidateErrors) != 1 ||
				summary.CandidateErrors[0].TaskID != third.TaskID || !errors.Is(summary.CandidateErrors[0].Err, failure) {
				t.Fatal("退出前缺少本轮部分结果", summary, err)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 || !slices.Equal(executed, []uint64{first.TaskID, second.TaskID, third.TaskID}) {
			t.Fatal("实际停止原因、汇报次数或执行范围不符", executed, reports, err)
		}
	})
}

func TestRunMultipartUploadExpirationLoopRejectsInvalidInputs(t *testing.T) {
	runOnce := func(context.Context) (MultipartUploadExpirationBatchResult, error) {
		t.Fatal("无效调用执行了首轮")
		return MultipartUploadExpirationBatchResult{}, nil
	}
	report := func(MultipartUploadExpirationBatchResult, error) { t.Fatal("无效调用交付了汇报") }
	ctx := t.Context()
	for _, fixture := range []struct {
		name  string
		check func() error
	}{
		{"nil context", func() error { return RunMultipartUploadExpirationLoop(nil, time.Second, runOnce, report) }},
		{"nonpositive interval", func() error { return RunMultipartUploadExpirationLoop(ctx, 0, runOnce, report) }},
		{"missing round callback", func() error { return RunMultipartUploadExpirationLoop(ctx, time.Second, nil, report) }},
		{"missing report callback", func() error { return RunMultipartUploadExpirationLoop(ctx, time.Second, runOnce, nil) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := fixture.check(); err == nil {
				t.Fatal("无效调用被接受")
			}
		})
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := RunMultipartUploadExpirationLoop(canceled, time.Second, runOnce, report); !errors.Is(err, context.Canceled) {
		t.Fatal("未保留预先取消的原因", err)
	}
}
