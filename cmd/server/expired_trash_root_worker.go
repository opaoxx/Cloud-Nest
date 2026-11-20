package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// startServerExpiredTrashRootWorker 启动到期标记循环，返回取消并等待退出的方法。
func startServerExpiredTrashRootWorker(
	ctx context.Context,
	runOnce func(context.Context, int) (service.ExpiredTrashRootBatchResult, error),
	logger *slog.Logger,
) func() {
	// 1. 从服务上下文派生停止信号。
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 启动到期标记循环，汇报本轮结果和异常。
	const interval = service.DefaultExpiredTrashRootInterval
	const batchLimit = domain.MaxExpiredTrashRootCandidates
	logger.Info("回收站到期标记 worker 已启动", "interval_ms", interval.Milliseconds(), "batch_limit", batchLimit)
	go func() {
		defer close(done)
		defer logger.Info("回收站到期标记 worker 已停止")
		var roundStartedAt time.Time
		err := service.RunExpiredTrashRootLoop(workerContext, interval,
			func(roundContext context.Context) (service.ExpiredTrashRootBatchResult, error) {
				roundStartedAt = time.Now()
				return runOnce(roundContext, batchLimit)
			},
			func(summary service.ExpiredTrashRootBatchResult, roundError error) {
				reportExpiredTrashRootRound(logger, summary, roundError, workerContext.Err(), time.Since(roundStartedAt))
			})
		if err != nil && workerContext.Err() == nil {
			logger.Error("回收站到期标记 worker 异常退出", "error_code", expiredTrashRootWorkerErrorCode(err))
		}
	}()

	// 3. 关停时取消循环，等待最后一轮汇报及退出。
	return func() {
		cancel()
		<-done
	}
}

// reportExpiredTrashRootRound 写入轮次汇总及逐项失败的固定错误码。
func reportExpiredTrashRootRound(logger *slog.Logger, summary service.ExpiredTrashRootBatchResult, roundError, stopError error, duration time.Duration) {
	// 1. 报告轮次和候选错误，过滤关停引起的告警。
	if roundError != nil && (stopError == nil || !errors.Is(roundError, stopError)) {
		logger.Warn("回收站到期标记轮次失败", "error_code", expiredTrashRootWorkerErrorCode(roundError))
	}
	for _, failure := range summary.CandidateErrors {
		if stopError != nil && errors.Is(failure.Err, stopError) {
			continue
		}
		logger.Warn("回收站到期根项标记失败", "user_id", failure.UserID, "root_id", failure.RootID,
			"error_code", expiredTrashRootWorkerErrorCode(failure.Err))
	}

	// 2. 汇报扫描、标记、失效和失败的数量。
	logger.Info("回收站到期标记轮次结束", "duration_ms", duration.Milliseconds(),
		"scanned", summary.Scanned, "marked", summary.Marked, "stale_candidates", summary.StaleCandidates,
		"candidate_errors", len(summary.CandidateErrors))
}

// expiredTrashRootWorkerErrorCode 补充到期标记的已知错误并复用根项的通用映射。
func expiredTrashRootWorkerErrorCode(err error) string {
	// 1. 区分影响单项标记的版本和归属错误。
	switch {
	case errors.Is(err, domain.ErrEntryRevisionExhausted):
		return "ENTRY_REVISION_EXHAUSTED"
	case errors.Is(err, domain.ErrAccountNotFound):
		return "ACCOUNT_NOT_FOUND"
	default:
		return purgeRootWorkerErrorCode(err)
	}
}
