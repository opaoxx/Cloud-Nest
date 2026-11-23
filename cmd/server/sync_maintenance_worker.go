package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type syncMaintenanceWorkerStore interface {
	offlineSyncDeviceWorkerStore
	syncChangePruneWorkerStore
}

// startServerSyncMaintenanceWorker 启动有序同步维护，返回取消并等待退出的方法。
func startServerSyncMaintenanceWorker(ctx context.Context, store syncMaintenanceWorkerStore, logger *slog.Logger) func() {
	// 1. 装配两个单轮服务，创建共用的取消信号。
	offlineBatch := service.NewOfflineSyncDeviceBatch(store.ReadOfflineSyncDeviceCutoff,
		store.ListOfflineSyncDeviceCandidates, store.MarkOfflineSyncDeviceResetRequired)
	pruneBatch := service.NewSyncChangePruneBatch(store.ReadSyncChangePruneCutoff,
		store.ListSyncChangePruneCandidates, store.PruneSyncChangeLogBatch)
	limits := syncChangePruneWorkerLimits()
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 按顺序执行两个阶段，分别汇报并保存续扫位置。
	logger.Info("同步维护 worker 已启动", "interval_ms", offlineSyncDeviceInterval.Milliseconds(),
		"offline_batch_limit", offlineSyncDeviceBatchLimit, "offline_page_size", offlineSyncDevicePageSize,
		"scan_limit", limits.ScanLimit, "page_size", limits.PageSize, "account_delete_limit", limits.AccountDeleteLimit)
	go func() {
		defer close(done)
		defer cancel()
		defer logger.Info("同步维护 worker 已停止")
		var afterDeviceID uint64
		var pruneStarted bool
		var pruneStartedAt time.Time
		err := service.RunSyncChangePruneLoop(workerContext, offlineSyncDeviceInterval,
			func(roundContext context.Context, afterUserID uint64) (service.SyncChangePruneBatchResult, error) {
				result, err := runServerSyncMaintenanceRound(roundContext, afterDeviceID, afterUserID,
					func(offlineContext context.Context, after uint64) (service.OfflineSyncDeviceBatchResult, error) {
						startedAt := time.Now()
						summary, err := offlineBatch.RunOnce(offlineContext, after, offlineSyncDeviceBatchLimit, offlineSyncDevicePageSize)
						reportOfflineSyncDeviceRound(logger, summary, err, offlineContext.Err(), time.Since(startedAt))
						return summary, err
					},
					func(pruneContext context.Context, after uint64) (service.SyncChangePruneBatchResult, error) {
						pruneStartedAt = time.Now()
						return pruneBatch.RunOnce(pruneContext, after, limits)
					})
				afterDeviceID = result.Offline.NextAfterDeviceID
				pruneStarted = result.PruneStarted
				return result.Prune, err
			},
			func(summary service.SyncChangePruneBatchResult, roundError error) {
				if pruneStarted {
					reportSyncChangePruneRound(logger, summary, roundError, workerContext.Err(), time.Since(pruneStartedAt))
				} else if workerContext.Err() == nil {
					logger.Info("同步日志裁剪本轮跳过", "next_after_user_id", summary.NextAfterUserID)
				}
			})
		if code := syncChangePruneWorkerReportCode(err, workerContext.Err()); code != "" {
			logger.Error("同步维护 worker 异常退出", "error_code", code)
		}
	}()

	// 3. 两阶段结束后等待一小时，关停时等待整个任务退出。
	return func() {
		cancel()
		<-done
	}
}
