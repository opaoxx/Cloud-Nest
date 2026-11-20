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

func TestRunExpiredTrashRootLoopWaitsAfterReportingAndContinuesAfterFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮执行超过一小时后失败，汇报完成后才开始等待。
		const interval = time.Hour
		if DefaultExpiredTrashRootInterval != interval {
			t.Fatal("到期标记默认间隔应为一小时", DefaultExpiredTrashRootInterval)
		}
		const firstWork = interval + 2*time.Second
		const firstReport = time.Second
		const secondStart = firstWork + firstReport + interval
		const stopAfter = secondStart + time.Second
		ctx, cancel := context.WithTimeout(t.Context(), stopAfter)
		defer cancel()
		scanFailure := errors.New("测试到期扫描故障")
		expected := ExpiredTrashRootBatchResult{
			Scanned: 3, Marked: 1, StaleCandidates: 1,
			CandidateErrors: []PurgeRootCandidateError{{UserID: 7, RootID: 33, Err: domain.ErrEntryRevisionExhausted}},
		}
		start := time.Now()
		var roundStarts []time.Duration
		reports := 0
		err := RunExpiredTrashRootLoop(ctx, DefaultExpiredTrashRootInterval, func(roundContext context.Context) (ExpiredTrashRootBatchResult, error) {
			if roundContext != ctx || len(roundStarts) != reports {
				t.Fatal("单轮应收到原上下文，并在上一轮汇报后开始")
			}
			roundStarts = append(roundStarts, time.Since(start))
			if len(roundStarts) == 1 {
				time.Sleep(firstWork)
				return ExpiredTrashRootBatchResult{}, scanFailure
			}
			return expected, nil
		}, func(summary ExpiredTrashRootBatchResult, roundError error) {
			reports++
			if reports == 1 {
				if !errors.Is(roundError, scanFailure) || summary.Scanned != 0 {
					t.Fatal("首轮扫描错误未交付", summary, roundError)
				}
				time.Sleep(firstReport)
				return
			}
			if roundError != nil || !reflect.DeepEqual(summary, expected) {
				t.Fatal("第二轮的标记、失效和失败结果未完整交付", summary, roundError)
			}
		})

		// 2. 首轮立即执行。第二轮须等首轮结束、汇报完成和完整一小时，随后取消等待。
		if !errors.Is(err, context.DeadlineExceeded) || reports != 2 || time.Since(start) != stopAfter ||
			!slices.Equal(roundStarts, []time.Duration{0, secondStart}) {
			t.Fatal("轮次重叠、等待间隔或退出时机不符", roundStarts, reports, time.Since(start), err)
		}
	})
}

func TestRunExpiredTrashRootLoopReportsPartialBatchOnStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 单轮先成功标记一项，第二项等到取消，第三项留待后续处理。
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		deletedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		candidates := []domain.ExpiredTrashRootCandidate{
			{EntryID: 31, UserID: 7, ParentID: 11, Revision: 2, DeletedAt: deletedAt},
			{EntryID: 32, UserID: 7, ParentID: 11, Revision: 3, DeletedAt: deletedAt},
			{EntryID: 33, UserID: 7, ParentID: 11, Revision: 4, DeletedAt: deletedAt},
		}
		var processed []uint64
		batch := NewExpiredTrashRootBatch(func(context.Context, int) ([]domain.ExpiredTrashRootCandidate, error) {
			return candidates, nil
		}, func(roundContext context.Context, candidate domain.ExpiredTrashRootCandidate) (*domain.PurgeRootCandidate, error) {
			processed = append(processed, candidate.EntryID)
			if candidate.EntryID == candidates[0].EntryID {
				return &domain.PurgeRootCandidate{
					EntryID: candidate.EntryID, UserID: candidate.UserID, ParentID: candidate.ParentID,
					Revision: candidate.Revision + 1, PurgeStartedAt: deletedAt.AddDate(0, 0, 31),
				}, nil
			}
			<-roundContext.Done()
			return nil, roundContext.Err()
		})
		reports := 0
		err := RunExpiredTrashRootLoop(ctx, DefaultExpiredTrashRootInterval, func(roundContext context.Context) (ExpiredTrashRootBatchResult, error) {
			return batch.RunOnce(roundContext, len(candidates))
		}, func(summary ExpiredTrashRootBatchResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.DeadlineExceeded) || summary.Scanned != 3 || summary.Marked != 1 ||
				summary.StaleCandidates != 0 || len(summary.CandidateErrors) != 1 {
				t.Fatal("退出前未交付已标记结果和取消原因", summary, roundError)
			}
			failure := summary.CandidateErrors[0]
			if failure.UserID != candidates[1].UserID || failure.RootID != candidates[1].EntryID ||
				!errors.Is(failure.Err, context.DeadlineExceeded) {
				t.Fatal("未保留在途候选的取消原因", failure)
			}
		})

		// 2. 循环在汇报后退出，取消后没有启动第三项或下一轮。
		if !errors.Is(err, context.DeadlineExceeded) || reports != 1 || !slices.Equal(processed, []uint64{31, 32}) {
			t.Fatal("取消后执行顺序、汇报次数或退出原因不符", processed, reports, err)
		}
	})
}
