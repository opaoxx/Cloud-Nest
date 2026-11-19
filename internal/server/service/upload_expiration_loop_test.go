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

func TestRunUploadExpirationLoopReportsAndWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 准备一次慢扫描失败和下一轮的混合处理结果。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const interval = 30 * time.Second
		scanFailure := errors.New("fixture expiry scan failure")
		first := domain.UploadExpirationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, UploadMode: "SINGLE"}
		second := domain.UploadExpirationCandidate{TaskID: 20, UserID: 7, StagedObjectID: 24, UploadMode: "SINGLE"}
		start := time.Now()
		var scanStarts []time.Duration
		var processed []uint64
		batch := NewUploadExpirationBatch(func(roundContext context.Context, limit int) ([]domain.UploadExpirationCandidate, error) {
			if roundContext != ctx || limit != 2 {
				t.Fatal("过期扫描没收到当前上下文或批量上限")
			}
			scanStarts = append(scanStarts, time.Since(start))
			if len(scanStarts) == 1 {
				time.Sleep(35 * time.Second)
				return nil, scanFailure
			}
			return []domain.UploadExpirationCandidate{first, second}, nil
		}, func(_ context.Context, candidate domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
			processed = append(processed, candidate.TaskID)
			if candidate == first {
				return nil, domain.ErrStoredObjectInUse
			}
			return &domain.UploadTask{ID: second.TaskID, UserID: second.UserID, StagedObjectID: second.StagedObjectID,
				Status: domain.UploadStatusExpired, UploadMode: second.UploadMode}, nil
		})

		// 2. 运行实际批次并核对每轮交付的汇总。
		reports := 0
		err := RunUploadExpirationLoop(ctx, interval, func(roundContext context.Context) (UploadExpirationBatchResult, error) {
			return batch.RunOnce(roundContext, 2)
		}, func(summary UploadExpirationBatchResult, roundError error) {
			reports++
			if reports == 1 {
				if !errors.Is(roundError, scanFailure) || summary.Scanned != 0 {
					t.Fatal("扫描错误没有交给回调", summary, roundError)
				}
				time.Sleep(time.Second)
				return
			}
			if roundError != nil || summary.Scanned != 2 || summary.Expired != 1 || len(summary.CandidateErrors) != 1 ||
				summary.CandidateErrors[0].TaskID != first.TaskID || !errors.Is(summary.CandidateErrors[0].Err, domain.ErrStoredObjectInUse) {
				t.Fatal("过期批次的汇总未完整交付", summary, roundError)
			}
			cancel()
		})
		if !errors.Is(err, context.Canceled) || reports != 2 || !slices.Equal(scanStarts, []time.Duration{0, 66 * time.Second}) ||
			!slices.Equal(processed, []uint64{first.TaskID, second.TaskID}) {
			t.Fatal("立即首轮、轮间等待或处理顺序不符", scanStarts, processed, reports, err)
		}
	})
}

func TestRunUploadExpirationLoopStopsWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rounds, reports := 0, 0
		finished := make(chan error, 1)
		start := time.Now()
		go func() {
			finished <- RunUploadExpirationLoop(ctx, 30*time.Second, func(context.Context) (UploadExpirationBatchResult, error) {
				rounds++
				return UploadExpirationBatchResult{}, nil
			}, func(UploadExpirationBatchResult, error) {
				reports++
			})
		}()
		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		err := <-finished
		if !errors.Is(err, context.Canceled) || rounds != 1 || reports != 1 || time.Since(start) != time.Second {
			t.Fatal("取消未中断过期轮间等待", rounds, reports, time.Since(start), err)
		}
	})
}

func TestRunUploadExpirationLoopReportsPartialBatchOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		first := domain.UploadExpirationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, UploadMode: "SINGLE"}
		second := domain.UploadExpirationCandidate{TaskID: 20, UserID: 7, StagedObjectID: 24, UploadMode: "SINGLE"}
		third := domain.UploadExpirationCandidate{TaskID: 21, UserID: 7, StagedObjectID: 25, UploadMode: "SINGLE"}
		var processed []uint64
		batch := NewUploadExpirationBatch(func(context.Context, int) ([]domain.UploadExpirationCandidate, error) {
			return []domain.UploadExpirationCandidate{first, second, third}, nil
		}, func(roundContext context.Context, candidate domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
			processed = append(processed, candidate.TaskID)
			if candidate == first {
				return &domain.UploadTask{ID: first.TaskID, UserID: first.UserID, StagedObjectID: first.StagedObjectID,
					Status: domain.UploadStatusExpired, UploadMode: first.UploadMode}, nil
			}
			<-roundContext.Done()
			return nil, roundContext.Err()
		})
		reports := 0
		err := RunUploadExpirationLoop(ctx, 30*time.Second, func(roundContext context.Context) (UploadExpirationBatchResult, error) {
			return batch.RunOnce(roundContext, 3)
		}, func(summary UploadExpirationBatchResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.DeadlineExceeded) || summary.Scanned != 3 || summary.Expired != 1 ||
				len(summary.CandidateErrors) != 1 || summary.CandidateErrors[0].TaskID != second.TaskID ||
				!errors.Is(summary.CandidateErrors[0].Err, context.DeadlineExceeded) {
				t.Fatal("运行中到期丢失了部分汇总", summary, roundError)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 || !slices.Equal(processed, []uint64{first.TaskID, second.TaskID}) {
			t.Fatal("任务到期后未停止过期处理循环", processed, reports, err)
		}
	})
}
