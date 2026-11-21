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

func TestRunUploadMergeLoopWaitsAfterReporting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const interval = 5 * time.Second
		scanError := errors.New("fixture scan error")
		candidateError := errors.New("fixture candidate error")
		result := UploadMergeBatchResult{
			Scanned: 6, Verifying: 1, Reopened: 1, Failed: 1, RetriesScheduled: 1, Skipped: 1,
			CandidateErrors: []UploadMergeCandidateError{{TaskID: 19, Err: candidateError}},
		}
		start := time.Now()
		var starts []time.Duration
		reports := 0

		// 1. 模拟首轮扫描耗时超过间隔，并让汇报本身也占用时间。
		err := RunUploadMergeLoop(ctx, interval, func(roundContext context.Context) (UploadMergeBatchResult, error) {
			if roundContext != ctx {
				t.Fatal("未向合并单轮传递原上下文")
			}
			starts = append(starts, time.Since(start))
			if len(starts) == 1 {
				time.Sleep(7 * time.Second)
				return UploadMergeBatchResult{}, scanError
			}
			return result, nil
		}, func(summary UploadMergeBatchResult, roundError error) {
			reports++
			if reports == 1 {
				if !errors.Is(roundError, scanError) || summary.Scanned != 0 {
					t.Fatal("首轮扫描错误未完整汇报", summary, roundError)
				}
				time.Sleep(time.Second)
				return
			}
			if roundError != nil || !reflect.DeepEqual(summary, result) {
				t.Fatal("本轮合并汇总未完整交付", summary, roundError)
			}
			cancel()
		})

		// 2. 首轮立即执行，下一轮在执行、汇报和完整间隔之后开始。
		if !errors.Is(err, context.Canceled) || reports != 2 || !slices.Equal(starts, []time.Duration{0, 13 * time.Second}) {
			t.Fatal("定时执行和汇报顺序不符", starts, reports, err)
		}
	})
}

func TestRunUploadMergeLoopStopsWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		rounds, reports := 0, 0
		finished := make(chan error, 1)
		go func() {
			finished <- RunUploadMergeLoop(ctx, 5*time.Second, func(context.Context) (UploadMergeBatchResult, error) {
				rounds++
				return UploadMergeBatchResult{}, nil
			}, func(UploadMergeBatchResult, error) {
				reports++
			})
		}()

		// 1. 等循环进入轮间等待，在下一轮到期前取消。
		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) || rounds != 1 || reports != 1 || time.Since(start) != time.Second {
			t.Fatal("循环未在取消后及时退出", rounds, reports, time.Since(start), err)
		}
	})
}

func TestRunUploadMergeLoopReportsPartialBatchOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		candidates := []domain.MultipartUploadCompletionCandidate{
			{TaskID: 1, UserID: 7, StagedObjectID: 21},
			{TaskID: 2, UserID: 7, StagedObjectID: 22},
		}
		batch := NewUploadMergeBatch(func(context.Context, int) ([]domain.MultipartUploadCompletionCandidate, error) {
			return candidates, nil
		}, func(_ context.Context, candidate domain.MultipartUploadCompletionCandidate, owner string) (*domain.UploadTask, error) {
			return uploadMergeBatchClaimFixture(candidate, owner, time.Now().UTC()), nil
		}, func(roundContext context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
			if claimed.ID == candidates[0].TaskID {
				result := copyUploadTask(claimed)
				result.Status, result.LeaseOwner, result.LeaseUntil = domain.UploadStatusVerifying, nil, nil
				return result, nil
			}
			<-roundContext.Done()
			return nil, roundContext.Err()
		})

		// 1. 调用真实批处理入口，首项提交后让第二项等到上下文到期。
		reports := 0
		err := RunUploadMergeLoop(ctx, 5*time.Second, func(roundContext context.Context) (UploadMergeBatchResult, error) {
			return batch.RunOnce(roundContext, 2, "fixture-merge-worker")
		}, func(summary UploadMergeBatchResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.DeadlineExceeded) || !reflect.DeepEqual(summary, UploadMergeBatchResult{Scanned: 2, Verifying: 1}) {
				t.Fatal("到期时没有汇报本轮已提交结果", summary, roundError)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 {
			t.Fatal("循环未在汇报部分结果后退出", reports, err)
		}
	})
}
