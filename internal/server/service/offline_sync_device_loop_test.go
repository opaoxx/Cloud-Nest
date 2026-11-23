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

func TestRunOfflineSyncDeviceLoopReportsWaitsAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮运行超过一个间隔，并在汇报中继续消耗虚拟时间。
		const interval = time.Hour
		const firstWork = interval + time.Minute
		const firstReport = 2 * time.Minute
		const secondStart = firstWork + firstReport + interval
		const stopAfter = secondStart + 2*interval + time.Second
		ctx, cancel := context.WithTimeout(t.Context(), stopAfter)
		defer cancel()
		itemFailure := errors.New("合成单项标记失败")
		scanFailure := errors.New("合成后续页读取失败")
		rounds := []struct {
			after   uint64
			summary OfflineSyncDeviceBatchResult
			err     error
		}{
			{after: 0, summary: OfflineSyncDeviceBatchResult{
				Scanned: 3, Marked: 1, StaleCandidates: 1, NextAfterDeviceID: 12,
				CandidateErrors: []OfflineSyncDeviceCandidateError{{UserID: 7, DeviceID: 10, Err: itemFailure}},
			}},
			{after: 12, summary: OfflineSyncDeviceBatchResult{Scanned: 1, Marked: 1, NextAfterDeviceID: 20}, err: scanFailure},
			{after: 20, summary: OfflineSyncDeviceBatchResult{}},
			{after: 0, summary: OfflineSyncDeviceBatchResult{}},
		}
		start := time.Now()
		var roundStarts []time.Duration
		reports := 0
		err := RunOfflineSyncDeviceLoop(ctx, interval, func(roundContext context.Context, afterDeviceID uint64) (OfflineSyncDeviceBatchResult, error) {
			if roundContext != ctx || len(roundStarts) != reports || reports >= len(rounds) || afterDeviceID != rounds[reports].after {
				t.Fatalf("上下文、串行顺序或续扫位置不符：轮次 %d，汇报 %d，位置 %d", len(roundStarts), reports, afterDeviceID)
			}
			roundStarts = append(roundStarts, time.Since(start))
			if reports == 0 {
				time.Sleep(firstWork)
			}
			return rounds[reports].summary, rounds[reports].err
		}, func(summary OfflineSyncDeviceBatchResult, roundError error) {
			if reports >= len(rounds) || !reflect.DeepEqual(summary, rounds[reports].summary) || !errors.Is(roundError, rounds[reports].err) {
				t.Fatalf("第 %d 次汇报未交付本轮结果：%+v，错误 %v", reports+1, summary, roundError)
			}
			if reports == 0 {
				time.Sleep(firstReport)
			}
			reports++
		})

		// 2. 确认汇报后才计时，部分失败后保留进度，扫完后归零。
		wantStarts := []time.Duration{0, secondStart, secondStart + interval, secondStart + 2*interval}
		if !errors.Is(err, context.DeadlineExceeded) || reports != len(rounds) ||
			time.Since(start) != stopAfter || !slices.Equal(roundStarts, wantStarts) {
			t.Fatalf("首轮、轮间等待或退出时机不符：%v，汇报 %d，耗时 %v，错误 %v", roundStarts, reports, time.Since(start), err)
		}
	})
}

func TestRunOfflineSyncDeviceLoopReportsProgressBeforeStopping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 使用真实单轮入口。第一项成功，第二项处理时取消。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		cutoff := time.Date(2026, 8, 25, 12, 0, 0, 15625000, time.UTC)
		oldActivity := cutoff.Add(-time.Microsecond)
		generation := domain.SyncCursorGeneration{1}
		candidates := []domain.OfflineSyncDeviceCandidate{
			{UserID: 7, DeviceID: 12, Generation: generation, LastSeenAt: oldActivity},
			{UserID: 7, DeviceID: 13, Generation: generation, LastSeenAt: oldActivity},
			{UserID: 7, DeviceID: 14, Generation: generation, LastSeenAt: oldActivity},
		}
		var attempts []uint64
		reads := 0
		batch := NewOfflineSyncDeviceBatch(func(context.Context) (time.Time, error) {
			reads++
			return cutoff, nil
		}, func(context.Context, time.Time, uint64, int) ([]domain.OfflineSyncDeviceCandidate, error) {
			return candidates, nil
		}, func(roundContext context.Context, _ time.Time, candidate domain.OfflineSyncDeviceCandidate) error {
			attempts = append(attempts, candidate.DeviceID)
			if candidate.DeviceID == 13 {
				cancel()
			}
			return roundContext.Err()
		})

		// 2. 先汇报已提交进度和取消原因，确认循环立即退出。
		reports := 0
		start := time.Now()
		err := RunOfflineSyncDeviceLoop(ctx, time.Hour, func(roundContext context.Context, afterDeviceID uint64) (OfflineSyncDeviceBatchResult, error) {
			return batch.RunOnce(roundContext, afterDeviceID, 3, 3)
		}, func(summary OfflineSyncDeviceBatchResult, roundError error) {
			reports++
			want := OfflineSyncDeviceBatchResult{Cutoff: cutoff, Scanned: 3, Marked: 1, NextAfterDeviceID: 12}
			if !errors.Is(roundError, context.Canceled) || !reflect.DeepEqual(summary, want) {
				t.Fatalf("取消时未交付本轮部分结果：%+v，错误 %v", summary, roundError)
			}
		})
		if !errors.Is(err, context.Canceled) || reports != 1 || reads != 1 ||
			time.Since(start) != 0 || !slices.Equal(attempts, []uint64{12, 13}) {
			t.Fatalf("取消后的执行或退出不符：尝试 %v，读钟 %d，汇报 %d，错误 %v", attempts, reads, reports, err)
		}
	})
}

func TestRunOfflineSyncDeviceLoopRejectsInvalidInputs(t *testing.T) {
	runOnce := func(context.Context, uint64) (OfflineSyncDeviceBatchResult, error) {
		t.Fatal("非法输入或已取消上下文进入了单轮")
		return OfflineSyncDeviceBatchResult{}, nil
	}
	report := func(OfflineSyncDeviceBatchResult, error) { t.Fatal("未运行批次却产生汇报") }
	for _, err := range []error{
		RunOfflineSyncDeviceLoop(nil, time.Hour, runOnce, report),
		RunOfflineSyncDeviceLoop(t.Context(), 0, runOnce, report),
		RunOfflineSyncDeviceLoop(t.Context(), time.Hour, nil, report),
		RunOfflineSyncDeviceLoop(t.Context(), time.Hour, runOnce, nil),
	} {
		if err == nil {
			t.Fatal("非法循环输入返回了成功")
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RunOfflineSyncDeviceLoop(canceled, time.Hour, runOnce, report); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的循环未返回取消原因：%v", err)
	}
}
