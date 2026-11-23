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

func TestRunSyncChangePruneLoopReportsWaitsAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮耗时超过一个间隔，汇报也消耗时间。模拟分页失败后继续并最终回到游标 0。
		const interval = time.Hour
		const firstWork = interval + time.Minute
		const firstReport = 2 * time.Minute
		const secondStart = firstWork + firstReport + interval
		const stopAfter = secondStart + 2*interval + time.Second
		ctx, cancel := context.WithTimeout(t.Context(), stopAfter)
		defer cancel()
		accountFailure := errors.New("合成账号失败")
		scanFailure := errors.New("合成后续页读取失败")
		rounds := []struct {
			after   uint64
			summary SyncChangePruneBatchResult
			err     error
		}{
			{after: 0, summary: SyncChangePruneBatchResult{
				Scanned: 4, PrunedAccounts: 1, DeletedEvents: 2, EmptyAccounts: 1, NextAfterUserID: 12,
				AccountErrors: []SyncChangePruneAccountError{{UserID: 10, Err: accountFailure}},
			}},
			{after: 12, summary: SyncChangePruneBatchResult{Scanned: 2, PrunedAccounts: 1, DeletedEvents: 1, NextAfterUserID: 20}, err: scanFailure},
			{after: 20, summary: SyncChangePruneBatchResult{}},
			{after: 0, summary: SyncChangePruneBatchResult{}},
		}
		start := time.Now()
		var starts []time.Duration
		reports := 0
		err := RunSyncChangePruneLoop(ctx, interval, func(roundContext context.Context, after uint64) (SyncChangePruneBatchResult, error) {
			if roundContext != ctx || len(starts) != reports || reports >= len(rounds) || after != rounds[reports].after {
				t.Fatalf("轮次顺序、上下文或续扫位置错误：轮次 %d，汇报 %d，位置 %d", len(starts), reports, after)
			}
			starts = append(starts, time.Since(start))
			if reports == 0 {
				time.Sleep(firstWork)
			}
			return rounds[reports].summary, rounds[reports].err
		}, func(summary SyncChangePruneBatchResult, roundError error) {
			if reports >= len(rounds) || !reflect.DeepEqual(summary, rounds[reports].summary) || !errors.Is(roundError, rounds[reports].err) {
				t.Fatalf("第 %d 次汇报未收到本轮结果：%+v，错误 %v", reports+1, summary, roundError)
			}
			if reports == 0 {
				time.Sleep(firstReport)
			}
			reports++
		})

		// 2. 间隔从汇报结束开始，等待时取消后立即退出。
		wantStarts := []time.Duration{0, secondStart, secondStart + interval, secondStart + 2*interval}
		if !errors.Is(err, context.DeadlineExceeded) || reports != len(rounds) ||
			!slices.Equal(starts, wantStarts) || time.Since(start) != stopAfter {
			t.Fatalf("首轮、间隔或退出时机错误：%v，汇报 %d，耗时 %v，错误 %v", starts, reports, time.Since(start), err)
		}
	})
}

func TestRunSyncChangePruneLoopReportsProgressBeforeStopping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 调用实际单轮服务，在第二个账号处理中取消。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 15625000, time.UTC)
		var attempted []uint64
		batch := NewSyncChangePruneBatch(func(context.Context) (time.Time, error) { return cutoff, nil },
			func(context.Context, time.Time, uint64, int) (domain.SyncChangePruneCandidatePage, error) {
				return domain.SyncChangePruneCandidatePage{UserIDs: []uint64{12, 13, 14}, NextAfterUserID: 14, ScannedCount: 3}, nil
			}, func(roundContext context.Context, userID uint64, _ time.Time, _ int) (domain.SyncChangePruneRange, error) {
				attempted = append(attempted, userID)
				if userID == 13 {
					cancel()
					return domain.SyncChangePruneRange{}, roundContext.Err()
				}
				return domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 18, Count: 1}, nil
			})

		// 2. 已提交的账号和续扫位置先汇报，循环不再等待下一个间隔。
		reports := 0
		start := time.Now()
		limits := SyncChangePruneBatchLimits{ScanLimit: 3, PageSize: 3, AccountDeleteLimit: 1}
		err := RunSyncChangePruneLoop(ctx, time.Hour, func(roundContext context.Context, after uint64) (SyncChangePruneBatchResult, error) {
			return batch.RunOnce(roundContext, after, limits)
		}, func(summary SyncChangePruneBatchResult, roundError error) {
			reports++
			want := SyncChangePruneBatchResult{Cutoff: cutoff, Scanned: 3, PrunedAccounts: 1, DeletedEvents: 1, NextAfterUserID: 12}
			if !errors.Is(roundError, context.Canceled) || !reflect.DeepEqual(summary, want) {
				t.Fatalf("取消时的部分结果不符：%+v，错误 %v", summary, roundError)
			}
		})
		if !errors.Is(err, context.Canceled) || reports != 1 || time.Since(start) != 0 ||
			!slices.Equal(attempted, []uint64{12, 13}) {
			t.Fatalf("取消后的执行或退出不符：尝试 %v，汇报 %d，错误 %v", attempted, reports, err)
		}
	})
}

func TestRunSyncChangePruneLoopRejectsInvalidInputs(t *testing.T) {
	runOnce := func(context.Context, uint64) (SyncChangePruneBatchResult, error) {
		t.Fatal("非法输入或已取消的循环仍执行单轮")
		return SyncChangePruneBatchResult{}, nil
	}
	report := func(SyncChangePruneBatchResult, error) { t.Fatal("未运行批次却产生汇报") }
	for _, err := range []error{
		RunSyncChangePruneLoop(nil, time.Hour, runOnce, report),
		RunSyncChangePruneLoop(t.Context(), 0, runOnce, report),
		RunSyncChangePruneLoop(t.Context(), time.Hour, nil, report),
		RunSyncChangePruneLoop(t.Context(), time.Hour, runOnce, nil),
	} {
		if err == nil {
			t.Fatal("非法循环输入返回成功")
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RunSyncChangePruneLoop(canceled, time.Hour, runOnce, report); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的循环未返回取消原因：%v", err)
	}
}
