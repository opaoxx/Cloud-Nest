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

func TestRunUploadCancellationLoopContinuesAfterScanError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮扫描失败，第二轮返回一条已取消和一条候选错误。
		const interval = 30 * time.Second
		const stopAfter = 34 * time.Second
		ctx, cancel := context.WithTimeout(t.Context(), stopAfter)
		defer cancel()
		scanFailure := errors.New("fixture cancellation scan failure")
		start := time.Now()
		var roundStarts []time.Duration
		reports := 0
		err := RunUploadCancellationLoop(ctx, interval, func(roundContext context.Context) (UploadCancellationBatchResult, error) {
			if roundContext != ctx {
				t.Fatal("单轮执行未收到调用方上下文")
			}
			roundStarts = append(roundStarts, time.Since(start))
			if len(roundStarts) == 1 {
				time.Sleep(2 * time.Second)
				return UploadCancellationBatchResult{}, scanFailure
			}
			return UploadCancellationBatchResult{
				Scanned: 2, Cancelled: 1,
				CandidateErrors: []UploadCancellationCandidateError{{TaskID: 19, Err: domain.ErrStoredObjectInUse}},
			}, nil
		}, func(summary UploadCancellationBatchResult, roundError error) {
			reports++
			if reports == 1 {
				if !errors.Is(roundError, scanFailure) || summary.Scanned != 0 {
					t.Fatal("首轮扫描错误未交付", summary, roundError)
				}
				time.Sleep(time.Second)
				return
			}
			if roundError != nil || summary.Scanned != 2 || summary.Cancelled != 1 ||
				len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != 19 ||
				!errors.Is(summary.CandidateErrors[0].Err, domain.ErrStoredObjectInUse) {
				t.Fatal("下一轮的取消汇总未完整交付", summary, roundError)
			}
		})

		// 2. 首轮立即执行，执行及汇报共 3 秒，之后等待 30 秒。停止打断下一次等待。
		if !errors.Is(err, context.DeadlineExceeded) || reports != 2 || time.Since(start) != stopAfter ||
			!slices.Equal(roundStarts, []time.Duration{0, 33 * time.Second}) {
			t.Fatal("轮间等待或停止时间不符", roundStarts, reports, time.Since(start), err)
		}
	})
}

func TestRunUploadCancellationLoopReportsPartialBatchOnStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 真实批次逻辑先处理一条，第二条等待停止，第三条留给后续轮次。
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		first := domain.UploadCancellationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23}
		second := domain.UploadCancellationCandidate{TaskID: 20, UserID: 7, StagedObjectID: 24}
		third := domain.UploadCancellationCandidate{TaskID: 21, UserID: 7, StagedObjectID: 25}
		var processed []uint64
		batch := NewUploadCancellationBatch(func(context.Context, int) ([]domain.UploadCancellationCandidate, error) {
			return []domain.UploadCancellationCandidate{first, second, third}, nil
		}, func(roundContext context.Context, candidate domain.UploadCancellationCandidate) (*domain.UploadTask, error) {
			processed = append(processed, candidate.TaskID)
			if candidate == first {
				return cancelledCandidateResult(first), nil
			}
			<-roundContext.Done()
			return nil, roundContext.Err()
		})

		// 2. 已提交结果和当前候选错误在循环返回前交付。
		reports := 0
		err := RunUploadCancellationLoop(ctx, 30*time.Second, func(roundContext context.Context) (UploadCancellationBatchResult, error) {
			return batch.RunOnce(roundContext, 3)
		}, func(summary UploadCancellationBatchResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.DeadlineExceeded) || summary.Scanned != 3 || summary.Cancelled != 1 ||
				len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != second.TaskID ||
				!errors.Is(summary.CandidateErrors[0].Err, context.DeadlineExceeded) {
				t.Fatal("停止时未交付已处理的部分结果", summary, roundError)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 || !slices.Equal(processed, []uint64{first.TaskID, second.TaskID}) {
			t.Fatal("停止后仍处理了剩余候选或未返回停止原因", processed, reports, err)
		}
	})
}
