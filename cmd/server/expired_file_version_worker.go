package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const expiredFileVersionInterval = time.Hour

// startServerExpiredFileVersionWorker 启动历史到期结算循环，返回取消并等待退出的方法。
func startServerExpiredFileVersionWorker(
	ctx context.Context,
	runOnce func(context.Context, int) (service.ExpiredFileVersionBatchResult, error),
	logger *slog.Logger,
) func() {
	// 1. 从服务上下文派生停止信号，并为本次运行创建退出通知。
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 按固定间隔和批量上限执行，记录每轮汇总及错误。
	const batchLimit = domain.MaxExpiredFileVersionCandidates
	logger.Info("历史到期结算 worker 已启动", "interval_ms", expiredFileVersionInterval.Milliseconds(), "batch_limit", batchLimit)
	go func() {
		defer close(done)
		defer logger.Info("历史到期结算 worker 已停止")
		var roundStartedAt time.Time
		err := service.RunExpiredFileVersionLoop(workerContext, expiredFileVersionInterval,
			func(roundContext context.Context) (service.ExpiredFileVersionBatchResult, error) {
				roundStartedAt = time.Now()
				return runOnce(roundContext, batchLimit)
			},
			func(summary service.ExpiredFileVersionBatchResult, roundError error) {
				reportExpiredFileVersionRound(logger, summary, roundError, workerContext.Err(), time.Since(roundStartedAt))
			})
		if code := expiredFileVersionWorkerReportCode(err, workerContext.Err()); code != "" {
			logger.Error("历史到期结算 worker 异常退出", "error_code", code)
		}
	}()

	// 3. 关停时发出取消，等待在途轮次汇报并退出。
	return func() {
		cancel()
		<-done
	}
}

func reportExpiredFileVersionRound(logger *slog.Logger, summary service.ExpiredFileVersionBatchResult, roundError, stopError error, duration time.Duration) {
	// 1. 过滤正常关停信号，使用固定码报告轮次和逐项故障。
	if code := expiredFileVersionWorkerReportCode(roundError, stopError); code != "" {
		logger.Warn("历史到期结算轮次失败", "error_code", code)
	}
	for _, failure := range summary.CandidateErrors {
		if code := expiredFileVersionWorkerReportCode(failure.Err, stopError); code != "" {
			logger.Warn("到期历史版本结算失败", "user_id", failure.UserID, "version_id", failure.VersionID, "error_code", code)
		}
	}

	// 2. 写入轮次用时和本轮处理结果。
	logger.Info("历史到期结算轮次结束", "duration_ms", duration.Milliseconds(),
		"scanned", summary.Scanned, "settled", summary.Settled, "stale_candidates", summary.StaleCandidates,
		"candidate_errors", len(summary.CandidateErrors))
}

func expiredFileVersionWorkerReportCode(err, stopError error) string {
	if err == nil {
		return ""
	}
	// 1. 只忽略单独的关停信号，保留与关停同时发生的独立故障。
	cause := err
	for next := errors.Unwrap(cause); next != nil; next = errors.Unwrap(cause) {
		cause = next
	}
	if cause == stopError {
		return ""
	}

	// 2. 补充历史字段错误，复用对象回收的账号、对象及通用错误码。
	if errors.Is(err, domain.ErrInvalidFileVersionData) {
		return "INVALID_FILE_VERSION_DATA"
	}
	code := storedObjectGCWorkerErrorCode(err)
	if stopError != nil && errors.Is(err, stopError) && code == storedObjectGCWorkerErrorCode(stopError) {
		return "INTERNAL_ERROR"
	}
	return code
}
