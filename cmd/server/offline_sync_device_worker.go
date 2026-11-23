package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const (
	offlineSyncDeviceInterval   = time.Hour
	offlineSyncDeviceBatchLimit = domain.MaxOfflineSyncDeviceCandidates
	offlineSyncDevicePageSize   = 50
)

type offlineSyncDeviceWorkerStore interface {
	ReadOfflineSyncDeviceCutoff(context.Context) (time.Time, error)
	ListOfflineSyncDeviceCandidates(context.Context, time.Time, uint64, int) ([]domain.OfflineSyncDeviceCandidate, error)
	MarkOfflineSyncDeviceResetRequired(context.Context, time.Time, domain.OfflineSyncDeviceCandidate) error
}

var _ offlineSyncDeviceWorkerStore = (*repository.Store)(nil)

// startServerOfflineSyncDeviceWorker 装配离线重建循环，返回取消并等待退出的方法。
func startServerOfflineSyncDeviceWorker(ctx context.Context, store offlineSyncDeviceWorkerStore, logger *slog.Logger) func() {
	// 1. 装配单轮仓储依赖，派生取消信号和退出通知。
	batch := service.NewOfflineSyncDeviceBatch(store.ReadOfflineSyncDeviceCutoff,
		store.ListOfflineSyncDeviceCandidates, store.MarkOfflineSyncDeviceResetRequired)
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 运行串行循环，记录轮次汇总和固定错误码。
	logger.Info("离线设备重建 worker 已启动", "interval_ms", offlineSyncDeviceInterval.Milliseconds(),
		"batch_limit", offlineSyncDeviceBatchLimit, "page_size", offlineSyncDevicePageSize)
	go func() {
		defer close(done)
		defer cancel()
		defer logger.Info("离线设备重建 worker 已停止")
		var roundStartedAt time.Time
		err := service.RunOfflineSyncDeviceLoop(workerContext, offlineSyncDeviceInterval,
			func(roundContext context.Context, afterDeviceID uint64) (service.OfflineSyncDeviceBatchResult, error) {
				roundStartedAt = time.Now()
				return batch.RunOnce(roundContext, afterDeviceID, offlineSyncDeviceBatchLimit, offlineSyncDevicePageSize)
			},
			func(summary service.OfflineSyncDeviceBatchResult, roundError error) {
				reportOfflineSyncDeviceRound(logger, summary, roundError, workerContext.Err(), time.Since(roundStartedAt))
			})
		if code := offlineSyncDeviceWorkerReportCode(err, workerContext.Err()); code != "" {
			logger.Error("离线设备重建 worker 异常退出", "error_code", code)
		}
	}()

	// 3. 关停时发送取消，等待汇报完成和任务退出。
	return func() {
		cancel()
		<-done
	}
}

func reportOfflineSyncDeviceRound(
	logger *slog.Logger, summary service.OfflineSyncDeviceBatchResult, roundError, stopError error, duration time.Duration,
) {
	// 1. 汇报整轮及单项的固定错误码，过滤单独的关停信号。
	if code := offlineSyncDeviceWorkerReportCode(roundError, stopError); code != "" {
		logger.Warn("离线设备重建轮次失败", "error_code", code)
	}
	for _, failure := range summary.CandidateErrors {
		if code := offlineSyncDeviceWorkerReportCode(failure.Err, stopError); code != "" {
			logger.Warn("离线设备标记待重建失败", "user_id", failure.UserID, "device_id", failure.DeviceID, "error_code", code)
		}
	}

	// 2. 写入轮次用时、处理计数与续扫位置。
	fields := []any{"duration_ms", duration.Milliseconds(), "scanned", summary.Scanned, "marked", summary.Marked,
		"stale_candidates", summary.StaleCandidates, "candidate_errors", len(summary.CandidateErrors),
		"next_after_device_id", summary.NextAfterDeviceID}
	if !summary.Cutoff.IsZero() {
		fields = append(fields, "cutoff", summary.Cutoff)
	}
	logger.Info("离线设备重建轮次结束", fields...)
}

func offlineSyncDeviceWorkerReportCode(err, stopError error) string {
	switch {
	case errors.Is(err, domain.ErrUserTransactionCommitUnconfirmed):
		return "USER_TRANSACTION_COMMIT_UNCONFIRMED"
	case errors.Is(err, domain.ErrInvalidSyncDeviceData):
		return "INVALID_SYNC_DEVICE_DATA"
	default:
		return expiredFileVersionWorkerReportCode(err, stopError)
	}
}
