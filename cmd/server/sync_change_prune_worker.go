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

const syncChangePruneInterval = time.Hour

type syncChangePruneWorkerStore interface {
	ReadSyncChangePruneCutoff(context.Context) (time.Time, error)
	ListSyncChangePruneCandidates(context.Context, time.Time, uint64, int) (domain.SyncChangePruneCandidatePage, error)
	PruneSyncChangeLogBatch(context.Context, uint64, time.Time, int) (domain.SyncChangePruneRange, error)
}

var _ syncChangePruneWorkerStore = (*repository.Store)(nil)

func syncChangePruneWorkerLimits() service.SyncChangePruneBatchLimits {
	return service.SyncChangePruneBatchLimits{
		ScanLimit:          service.MaxSyncChangePruneAccountsPerRound,
		PageSize:           50,
		AccountDeleteLimit: domain.MaxSyncChangePruneBatchSize,
	}
}

// startServerSyncChangePruneWorker 装配同步日志裁剪循环，返回取消并等待退出的方法。
func startServerSyncChangePruneWorker(ctx context.Context, store syncChangePruneWorkerStore, logger *slog.Logger) func() {
	// 1. 绑定仓储入口和默认参数，创建取消信号。
	batch := service.NewSyncChangePruneBatch(store.ReadSyncChangePruneCutoff, store.ListSyncChangePruneCandidates, store.PruneSyncChangeLogBatch)
	limits := syncChangePruneWorkerLimits()
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 运行串行循环，记录轮次计数和固定错误码。
	logger.Info("同步日志裁剪 worker 已启动", "interval_ms", syncChangePruneInterval.Milliseconds(),
		"scan_limit", limits.ScanLimit, "page_size", limits.PageSize, "account_delete_limit", limits.AccountDeleteLimit)
	go func() {
		defer close(done)
		defer cancel()
		defer logger.Info("同步日志裁剪 worker 已停止")
		var roundStartedAt time.Time
		err := service.RunSyncChangePruneLoop(workerContext, syncChangePruneInterval,
			func(roundContext context.Context, afterUserID uint64) (service.SyncChangePruneBatchResult, error) {
				roundStartedAt = time.Now()
				return batch.RunOnce(roundContext, afterUserID, limits)
			},
			func(summary service.SyncChangePruneBatchResult, roundError error) {
				reportSyncChangePruneRound(logger, summary, roundError, workerContext.Err(), time.Since(roundStartedAt))
			})
		if code := syncChangePruneWorkerReportCode(err, workerContext.Err()); code != "" {
			logger.Error("同步日志裁剪 worker 异常退出", "error_code", code)
		}
	}()

	// 3. 收到停止请求后，等待在途轮次汇报并退出。
	return func() {
		cancel()
		<-done
	}
}

func reportSyncChangePruneRound(logger *slog.Logger, summary service.SyncChangePruneBatchResult, roundError, stopError error, duration time.Duration) {
	// 1. 用固定码记录整轮和单账号故障，忽略单独的正常关停信号。
	if code := syncChangePruneWorkerReportCode(roundError, stopError); code != "" {
		logger.Warn("同步日志裁剪轮次失败", "error_code", code)
	}
	for _, failure := range summary.AccountErrors {
		if code := syncChangePruneWorkerReportCode(failure.Err, stopError); code != "" {
			logger.Warn("同步日志裁剪账号失败", "user_id", failure.UserID, "error_code", code)
		}
	}

	// 2. 写入本轮耗时、处理计数和账号续扫位置。
	fields := []any{"duration_ms", duration.Milliseconds(), "scanned", summary.Scanned,
		"pruned_accounts", summary.PrunedAccounts, "deleted_events", summary.DeletedEvents, "empty_accounts", summary.EmptyAccounts,
		"account_errors", len(summary.AccountErrors), "next_after_user_id", summary.NextAfterUserID}
	if !summary.Cutoff.IsZero() {
		fields = append(fields, "cutoff", summary.Cutoff)
	}
	logger.Info("同步日志裁剪轮次结束", fields...)
}

func syncChangePruneWorkerReportCode(err, stopError error) string {
	switch {
	case errors.Is(err, domain.ErrSyncChangePruneCountMismatch):
		return "SYNC_CHANGE_PRUNE_COUNT_MISMATCH"
	case errors.Is(err, domain.ErrInvalidSyncSnapshotSequence):
		return "INVALID_SYNC_CHANGE_SEQUENCE"
	default:
		return offlineSyncDeviceWorkerReportCode(err, stopError)
	}
}
