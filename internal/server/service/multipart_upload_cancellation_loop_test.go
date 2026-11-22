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

func TestRunMultipartUploadCancellationLoopWaitsAfterReporting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const interval = 30 * time.Second
		scanFailure := errors.New("fixture cancellation scan failure")
		result := MultipartUploadCancellationBatchResult{
			Scanned: 4, Cancelled: 1, RetriesScheduled: 1, Skipped: 1,
			CandidateErrors: []UploadCancellationCandidateError{{TaskID: 20, Err: domain.ErrStoredObjectInUse}},
		}
		start := time.Now()
		var starts, reports []time.Duration

		// 1. 让失败的首轮执行时间超过间隔，汇报也消耗虚拟时间。
		err := RunMultipartUploadCancellationLoop(ctx, interval, func(roundContext context.Context) (MultipartUploadCancellationBatchResult, error) {
			if roundContext != ctx {
				t.Fatal("单轮未收到原上下文")
			}
			starts = append(starts, time.Since(start))
			if len(starts) == 1 {
				time.Sleep(35 * time.Second)
				return MultipartUploadCancellationBatchResult{}, scanFailure
			}
			return result, nil
		}, func(summary MultipartUploadCancellationBatchResult, roundError error) {
			reports = append(reports, time.Since(start))
			if len(reports) == 1 {
				if !errors.Is(roundError, scanFailure) || !reflect.DeepEqual(summary, MultipartUploadCancellationBatchResult{}) {
					t.Fatal("首轮错误未随汇总交付", summary, roundError)
				}
				time.Sleep(2 * time.Second)
				return
			}
			if roundError != nil || !reflect.DeepEqual(summary, result) {
				t.Fatal("未完整交付四类结果", summary, roundError)
			}
			cancel()
		})

		// 2. 第 0 秒执行首轮，第 37 秒完成汇报，第 67 秒才开始下一轮。
		if !errors.Is(err, context.Canceled) || !slices.Equal(starts, []time.Duration{0, 67 * time.Second}) ||
			!slices.Equal(reports, []time.Duration{35 * time.Second, 67 * time.Second}) || time.Since(start) != 67*time.Second {
			t.Fatal("执行、汇报或轮间等待时点不符", starts, reports, time.Since(start), err)
		}
	})
}

func TestRunMultipartUploadCancellationLoopStopsWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		rounds, reports := 0, 0
		finished := make(chan error, 1)
		go func() {
			finished <- RunMultipartUploadCancellationLoop(ctx, 30*time.Second,
				func(context.Context) (MultipartUploadCancellationBatchResult, error) {
					rounds++
					return MultipartUploadCancellationBatchResult{}, nil
				}, func(MultipartUploadCancellationBatchResult, error) { reports++ })
		}()

		// 1. 让循环进入等待后取消，核对下一轮没有开始。
		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) || rounds != 1 || reports != 1 || time.Since(start) != time.Second {
			t.Fatal("轮间取消未及时停止", rounds, reports, time.Since(start), err)
		}
	})
}

func TestRunMultipartUploadCancellationLoopReportsPartialBatchOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		const worker = "fixture-worker"
		failure := errors.New("fixture candidate failure")
		candidates := []domain.UploadCancellationCandidate{
			{TaskID: 19, UserID: 7, StagedObjectID: 23},
			{TaskID: 20, UserID: 7, StagedObjectID: 24},
			{TaskID: 21, UserID: 8, StagedObjectID: 25},
			{TaskID: 22, UserID: 8, StagedObjectID: 26},
		}
		var executed []uint64
		batch := NewMultipartUploadCancellationBatch(func(context.Context, int) ([]domain.UploadCancellationCandidate, error) {
			return candidates, nil
		}, func(_ context.Context, candidate domain.UploadCancellationCandidate, owner string) (*domain.UploadTask, error) {
			if candidate.TaskID == 22 || owner != worker {
				t.Fatal("领取了应留待下一轮的候选，或持有人不符")
			}
			return multipartCancellationBatchClaim(candidate, owner, time.Now().UTC()), nil
		}, func(roundContext context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
			executed = append(executed, claimed.ID)
			switch claimed.ID {
			case 19:
				return multipartCancellationBatchFinished(claimed), nil
			case 20:
				return nil, &MultipartUploadCancellationRetryScheduledError{
					Cause: domain.ErrUploadStorageUnavailable, NextAttemptAt: *claimed.LeaseUntil,
				}
			default:
				<-roundContext.Done()
				return nil, errors.Join(roundContext.Err(), failure)
			}
		})

		// 1. 真实单轮编排部分完成后停止，在退出前汇报已提交结果和独立故障。
		reports := 0
		err := RunMultipartUploadCancellationLoop(ctx, 30*time.Second, func(roundContext context.Context) (MultipartUploadCancellationBatchResult, error) {
			return batch.RunOnce(roundContext, len(candidates), worker)
		}, func(summary MultipartUploadCancellationBatchResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.DeadlineExceeded) || summary.Scanned != 4 ||
				summary.Cancelled != 1 || summary.RetriesScheduled != 1 || summary.Skipped != 0 ||
				len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != 21 ||
				!errors.Is(summary.CandidateErrors[0].Err, failure) {
				t.Fatal("到期时未交付完整的部分结果", summary, roundError)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 || !slices.Equal(executed, []uint64{19, 20, 21}) {
			t.Fatal("停止后仍执行了新任务或没有返回原停止原因", executed, reports, err)
		}
	})
}

func TestRunMultipartUploadCancellationLoopRejectsInvalidInputs(t *testing.T) {
	runOnce := func(context.Context) (MultipartUploadCancellationBatchResult, error) {
		t.Fatal("无效调用不应执行首轮")
		return MultipartUploadCancellationBatchResult{}, nil
	}
	report := func(MultipartUploadCancellationBatchResult, error) { t.Fatal("无效调用不应汇报") }
	for _, fixture := range []struct {
		name  string
		check func() error
	}{
		{"nil context", func() error { return RunMultipartUploadCancellationLoop(nil, time.Second, runOnce, report) }},
		{"nonpositive interval", func() error { return RunMultipartUploadCancellationLoop(t.Context(), 0, runOnce, report) }},
		{"missing round callback", func() error { return RunMultipartUploadCancellationLoop(t.Context(), time.Second, nil, report) }},
		{"missing reporting callback", func() error { return RunMultipartUploadCancellationLoop(t.Context(), time.Second, runOnce, nil) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := fixture.check(); err == nil {
				t.Fatal("无效调用被接受")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RunMultipartUploadCancellationLoop(ctx, time.Second, runOnce, report); !errors.Is(err, context.Canceled) {
		t.Fatal("预先取消时未保留停止原因", err)
	}
}
