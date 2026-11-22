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

func TestRunExpiredFileVersionLoopWaitsAfterReportingAndContinuesAfterFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 让首轮执行超过一个间隔，且汇报期间也继续占用循环。
		const interval = time.Hour
		const firstWork = interval + time.Minute
		const firstReport = 2 * time.Minute
		const nextStart = firstWork + firstReport + interval
		const stopAfter = nextStart + time.Second
		ctx, cancel := context.WithTimeout(t.Context(), stopAfter)
		defer cancel()
		scanFailure := errors.New("测试首轮扫描失败")
		itemFailure := errors.New("测试第二轮单项结算失败")
		want := ExpiredFileVersionBatchResult{
			Scanned: 3, Settled: 1, StaleCandidates: 1,
			CandidateErrors: []ExpiredFileVersionCandidateError{{UserID: 7, VersionID: 51, Err: itemFailure}},
		}
		var roundStarts []time.Duration
		reports := 0
		start := time.Now()
		err := RunExpiredFileVersionLoop(ctx, interval, func(roundContext context.Context) (ExpiredFileVersionBatchResult, error) {
			if roundContext != ctx || len(roundStarts) != reports {
				t.Fatal("单轮上下文错误或上一轮尚未汇报")
			}
			roundStarts = append(roundStarts, time.Since(start))
			if len(roundStarts) == 1 {
				time.Sleep(firstWork)
				return ExpiredFileVersionBatchResult{}, scanFailure
			}
			return want, nil
		}, func(summary ExpiredFileVersionBatchResult, roundError error) {
			reports++
			if reports == 1 {
				if !errors.Is(roundError, scanFailure) || summary.Scanned != 0 {
					t.Fatal("首轮扫描错误未交付", summary, roundError)
				}
				time.Sleep(firstReport)
				return
			}
			if roundError != nil || !reflect.DeepEqual(summary, want) {
				t.Fatal("第二轮汇总未完整交付", summary, roundError)
			}
		})

		// 2. 从汇报结束算起等待完整间隔，下一轮继续执行，等待中超时立即退出。
		if !errors.Is(err, context.DeadlineExceeded) || reports != 2 || time.Since(start) != stopAfter ||
			!slices.Equal(roundStarts, []time.Duration{0, nextStart}) {
			t.Fatal("首轮、轮间等待或退出时机不符", roundStarts, reports, time.Since(start), err)
		}
	})
}

func TestRunExpiredFileVersionLoopReportsPartialBatchOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		// 1. 使用单轮入口模拟第一项已提交、第二项在执行中被取消。
		candidates := []domain.FileVersion{{ID: 51}, {ID: 52}, {ID: 53}}
		var settledAttempts []uint64
		batch := NewExpiredFileVersionBatch(func(context.Context, int) ([]domain.FileVersion, error) {
			return candidates, nil
		}, func(roundContext context.Context, candidate domain.FileVersion) error {
			settledAttempts = append(settledAttempts, candidate.ID)
			if candidate.ID == 51 {
				return nil
			}
			if candidate.ID == 52 {
				cancel()
			}
			return roundContext.Err()
		})

		// 2. 取消时仍汇报已提交结果，第三项和下一轮均不执行。
		reports := 0
		err := RunExpiredFileVersionLoop(ctx, time.Hour, func(roundContext context.Context) (ExpiredFileVersionBatchResult, error) {
			return batch.RunOnce(roundContext, len(candidates))
		}, func(summary ExpiredFileVersionBatchResult, roundError error) {
			reports++
			if !errors.Is(roundError, context.Canceled) || summary.Scanned != 3 || summary.Settled != 1 ||
				summary.StaleCandidates != 0 || len(summary.CandidateErrors) != 0 {
				t.Fatal("未先汇报已结算结果和取消原因", summary, roundError)
			}
		})
		if !errors.Is(err, context.Canceled) || reports != 1 || !slices.Equal(settledAttempts, []uint64{51, 52}) {
			t.Fatal("取消后的结算、汇报或退出结果不符", settledAttempts, reports, err)
		}
	})
}
