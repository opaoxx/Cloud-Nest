package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

type offlineSyncDeviceWorkerStoreFixture struct {
	read func(context.Context) (time.Time, error)
	list func(context.Context, time.Time, uint64, int) ([]domain.OfflineSyncDeviceCandidate, error)
	mark func(context.Context, time.Time, domain.OfflineSyncDeviceCandidate) error
}

func (fixture offlineSyncDeviceWorkerStoreFixture) ReadOfflineSyncDeviceCutoff(ctx context.Context) (time.Time, error) {
	return fixture.read(ctx)
}

func (fixture offlineSyncDeviceWorkerStoreFixture) ListOfflineSyncDeviceCandidates(
	ctx context.Context, cutoff time.Time, afterDeviceID uint64, limit int,
) ([]domain.OfflineSyncDeviceCandidate, error) {
	return fixture.list(ctx, cutoff, afterDeviceID, limit)
}

func (fixture offlineSyncDeviceWorkerStoreFixture) MarkOfflineSyncDeviceResetRequired(
	ctx context.Context, cutoff time.Time, candidate domain.OfflineSyncDeviceCandidate,
) error {
	return fixture.mark(ctx, cutoff, candidate)
}

func TestServerOfflineSyncDeviceWorkerReportsAndWaitsForStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮读取失败，第二轮通过实际批次入口依次处理合成候选。
		var logs bytes.Buffer
		reachedLast, canceled := make(chan struct{}), make(chan struct{})
		release, stopped := make(chan struct{}), make(chan struct{})
		reads := 0
		start := time.Now()
		cutoff := time.Date(2026, 8, 25, 12, 0, 0, 15625000, time.UTC)
		fixture := offlineSyncDeviceWorkerStoreFixture{
			read: func(context.Context) (time.Time, error) {
				reads++
				if reads == 1 {
					if time.Since(start) != 0 {
						t.Error("首轮应立即执行")
					}
					return time.Time{}, errors.New("fixture private cutoff detail")
				}
				return cutoff, nil
			},
			list: func(_ context.Context, received time.Time, afterDeviceID uint64, limit int) ([]domain.OfflineSyncDeviceCandidate, error) {
				if received != cutoff || afterDeviceID != 0 || limit != 50 {
					t.Errorf("页参数不符：截止点 %v，位置 %d，上限 %d", received, afterDeviceID, limit)
				}
				var candidates []domain.OfflineSyncDeviceCandidate
				for _, deviceID := range []uint64{10, 11, 12, 13} {
					candidates = append(candidates, domain.OfflineSyncDeviceCandidate{
						UserID: 7, DeviceID: deviceID, LastSeenAt: cutoff.Add(-time.Microsecond), Generation: domain.SyncCursorGeneration{1},
					})
				}
				return candidates, nil
			},
			mark: func(roundContext context.Context, received time.Time, candidate domain.OfflineSyncDeviceCandidate) error {
				if received != cutoff {
					t.Errorf("单项标记的截止点不符：%v", received)
				}
				switch candidate.DeviceID {
				case 10:
					return fmt.Errorf("fixture private device detail: %w", domain.ErrInvalidSyncDeviceData)
				case 11:
					return domain.ErrOfflineSyncDeviceCandidateStale
				case 12:
					return nil
				}
				close(reachedLast)
				<-roundContext.Done()
				close(canceled)
				<-release
				time.Sleep(2 * time.Second)
				return errors.Join(domain.ErrUserTransactionCommitUnconfirmed, roundContext.Err(), errors.New("fixture private commit detail"))
			},
		}
		stop := startServerOfflineSyncDeviceWorker(t.Context(), fixture, logging.New(&logs))
		<-reachedLast
		if time.Since(start) != time.Hour {
			t.Error("第二轮应等待完整一小时", time.Since(start))
		}

		// 2. 停止方法先取消，再等待在途事务交付结果。
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("在途轮次尚未返回，停止方法已结束")
		default:
		}
		close(release)
		<-stopped
		stop()

		// 3. 核对结构化计数、续扫、固定错误码和停止顺序。
		type workerLog struct {
			Message           string    `json:"msg"`
			Level             string    `json:"level"`
			IntervalMS        int64     `json:"interval_ms"`
			BatchLimit        int       `json:"batch_limit"`
			PageSize          int       `json:"page_size"`
			DurationMS        int64     `json:"duration_ms"`
			Cutoff            time.Time `json:"cutoff"`
			Scanned           int       `json:"scanned"`
			Marked            int       `json:"marked"`
			StaleCandidates   int       `json:"stale_candidates"`
			CandidateErrors   int       `json:"candidate_errors"`
			NextAfterDeviceID uint64    `json:"next_after_device_id"`
			UserID            uint64    `json:"user_id"`
			DeviceID          uint64    `json:"device_id"`
			ErrorCode         string    `json:"error_code"`
		}
		var actual []workerLog
		for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
			var entry workerLog
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatal("解析 worker 日志失败", err)
			}
			actual = append(actual, entry)
		}
		want := []workerLog{
			{Message: "离线设备重建 worker 已启动", Level: "INFO", IntervalMS: 3600000, BatchLimit: 100, PageSize: 50},
			{Message: "离线设备重建轮次失败", Level: "WARN", ErrorCode: "INTERNAL_ERROR"},
			{Message: "离线设备重建轮次结束", Level: "INFO"},
			{Message: "离线设备标记待重建失败", Level: "WARN", UserID: 7, DeviceID: 10, ErrorCode: "INVALID_SYNC_DEVICE_DATA"},
			{Message: "离线设备标记待重建失败", Level: "WARN", UserID: 7, DeviceID: 13, ErrorCode: "USER_TRANSACTION_COMMIT_UNCONFIRMED"},
			{Message: "离线设备重建轮次结束", Level: "INFO", DurationMS: 2000, Cutoff: cutoff,
				Scanned: 4, Marked: 1, StaleCandidates: 1, CandidateErrors: 2, NextAfterDeviceID: 13},
			{Message: "离线设备重建 worker 已停止", Level: "INFO"},
		}
		if reads != 2 || !reflect.DeepEqual(actual, want) || strings.Contains(logs.String(), "fixture private") {
			t.Fatalf("日志或退出顺序不符，读钟 %d 次：%s", reads, logs.String())
		}
	})
}

func TestServerOfflineSyncDeviceWorkerWaitsForFinalReportOnServiceCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 父上下文触发关停，日志门闩暂停最后一轮汇报。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var logs bytes.Buffer
		gate := &serverWorkerReportGate{
			Handler: logging.New(&logs).Handler(), reportMessage: "离线设备重建轮次结束",
			reportStarted: make(chan struct{}), releaseReport: make(chan struct{}),
		}
		started, finished := make(chan struct{}), make(chan struct{})
		cutoff := time.Date(2026, 8, 25, 12, 0, 0, 15625000, time.UTC)
		fixture := offlineSyncDeviceWorkerStoreFixture{
			read: func(context.Context) (time.Time, error) { return cutoff, nil },
			list: func(context.Context, time.Time, uint64, int) ([]domain.OfflineSyncDeviceCandidate, error) {
				return []domain.OfflineSyncDeviceCandidate{{
					UserID: 7, DeviceID: 12, LastSeenAt: cutoff.Add(-time.Microsecond), Generation: domain.SyncCursorGeneration{1},
				}}, nil
			},
			mark: func(roundContext context.Context, _ time.Time, _ domain.OfflineSyncDeviceCandidate) error {
				close(started)
				<-roundContext.Done()
				return nil
			},
		}
		stop := startServerOfflineSyncDeviceWorker(ctx, fixture, slog.New(gate))
		<-started
		cancel()
		<-gate.reportStarted

		// 2. 等待函数必须等汇报完成，最后才写 worker 停止日志。
		go func() {
			stop()
			close(finished)
		}()
		synctest.Wait()
		select {
		case <-finished:
			t.Error("仍在汇报，停止方法已结束")
		default:
		}
		close(gate.releaseReport)
		<-finished
		logText := logs.String()
		report := strings.Index(logText, "离线设备重建轮次结束")
		stopped := strings.Index(logText, "离线设备重建 worker 已停止")
		if report < 0 || stopped <= report || !strings.Contains(logText, `"marked":1`) || strings.Contains(logText, `"level":"WARN"`) {
			t.Fatalf("父上下文取消后的日志顺序或结果不符：%s", logText)
		}
	})
}
