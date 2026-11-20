package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const purgeRootInterval = 5 * time.Second

// startServerPurgeRootWorker 启动永久清理循环，返回取消并等待退出的方法。
func startServerPurgeRootWorker(
	ctx context.Context,
	runOnce func(context.Context, int, int) (service.PurgeRootBatchResult, error),
	logger *slog.Logger,
) func() {
	// 1. 用服务上下文创建停止信号和完成通知。
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 启动清理循环，记录本轮汇总和固定错误码。
	logger.Info("永久清理 worker 已启动", "interval_ms", purgeRootInterval.Milliseconds(),
		"root_limit", domain.MaxPurgeRootCandidates, "leaf_limit", domain.MaxPurgeLeafCandidates)
	go func() {
		defer close(done)
		defer logger.Info("永久清理 worker 已停止")
		err := service.RunPurgeRootLoop(workerContext, purgeRootInterval,
			func(roundContext context.Context) (service.PurgeRootBatchResult, error) {
				return runOnce(roundContext, domain.MaxPurgeRootCandidates, domain.MaxPurgeLeafCandidates)
			},
			func(summary service.PurgeRootBatchResult, roundError error) {
				stopError := workerContext.Err()
				if roundError != nil && (stopError == nil || !errors.Is(roundError, stopError)) {
					logger.Warn("永久清理轮次失败", "error_code", purgeRootWorkerErrorCode(roundError))
				}
				for _, failure := range summary.CandidateErrors {
					if stopError != nil && errors.Is(failure.Err, stopError) {
						continue
					}
					logger.Warn("永久清理根项处理失败", "user_id", failure.UserID,
						"root_id", failure.RootID, "error_code", purgeRootWorkerErrorCode(failure.Err))
				}
				if summary.Scanned > 0 {
					logger.Info("永久清理轮次结束", "scanned", summary.Scanned, "completed_leaves", summary.CompletedLeaves,
						"stale_roots", summary.StaleRoots, "candidate_errors", len(summary.CandidateErrors))
				}
			})
		if err != nil && workerContext.Err() == nil {
			logger.Error("永久清理 worker 异常退出", "error_code", purgeRootWorkerErrorCode(err))
		}
	}()

	// 3. 关停时发出取消信号，等待循环退出后再释放数据库连接。
	return func() {
		cancel()
		<-done
	}
}

// purgeRootWorkerErrorCode 返回可写入日志的固定错误码。
func purgeRootWorkerErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		return "DEADLINE_EXCEEDED"
	case errors.Is(err, domain.ErrInvalidAccountData):
		return "INVALID_ACCOUNT_DATA"
	case errors.Is(err, domain.ErrInvalidFileEntryData):
		return "INVALID_FILE_ENTRY_DATA"
	default:
		return "INTERNAL_ERROR"
	}
}
