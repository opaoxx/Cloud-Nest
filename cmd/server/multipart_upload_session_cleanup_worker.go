package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// startServerMultipartUploadSessionCleanupWorker 启动会话收尾循环，返回取消并等待退出的方法。
func startServerMultipartUploadSessionCleanupWorker(
	ctx context.Context,
	interval time.Duration,
	runOnce func(context.Context, service.MultipartUploadSessionCleanupPosition) (service.MultipartUploadSessionCleanupRoundResult, error),
	logger *slog.Logger,
) func() {
	// 1. 派生取消信号，创建退出通知。
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// 2. 运行串行循环，汇报扫描进度和单项故障。
	logger.Info("分片会话收尾 worker 已启动", "interval_ms", interval.Milliseconds())
	go func() {
		defer close(done)
		defer cancel()
		defer logger.Info("分片会话收尾 worker 已停止")
		err := service.RunMultipartUploadSessionCleanupLoop(workerContext, interval, runOnce,
			func(summary service.MultipartUploadSessionCleanupRoundResult, roundError error) {
				reportMultipartUploadSessionCleanupRound(logger, summary, roundError, workerContext.Err())
			})
		if code := multipartUploadCleanupReportCode(err, workerContext.Err()); code != "" {
			logger.Error("分片会话收尾 worker 异常退出", "error_code", code)
		}
	}()

	// 3. 停止时取消在途轮次，等待汇报与退出。
	return func() {
		cancel()
		<-done
	}
}

func reportMultipartUploadSessionCleanupRound(
	logger *slog.Logger, summary service.MultipartUploadSessionCleanupRoundResult, roundError, stopError error,
) {
	// 1. 用固定错误码汇报整轮错误及各对象的查找错误。
	if code := multipartUploadCleanupReportCode(roundError, stopError); code != "" {
		logger.Warn("分片会话收尾轮次失败", "error_code", code)
	}
	scanned, scanErrors := 0, 0
	for _, object := range summary.Scan.Objects {
		scanned += object.Scan.Scanned
		for _, failures := range [][]service.MultipartUploadSessionScanFailure{object.Scan.Failures, object.Scan.InterruptedFailures} {
			scanErrors += len(failures)
			for _, failure := range failures {
				if code := multipartUploadCleanupReportCode(failure.Err, stopError); code != "" {
					logger.Warn("分片会话扫描复核失败", "task_id", object.Target.TaskID, "object_id", object.Target.ObjectID, "error_code", code)
				}
			}
		}
	}

	// 2. 汇报收尾失败，保留正常关停之外的独立故障。
	for _, item := range summary.Items {
		if code := multipartUploadCleanupReportCode(item.Err, stopError); code != "" {
			logger.Warn("分片会话候选收尾失败", "task_id", item.Target.TaskID, "object_id", item.Target.ObjectID, "error_code", code)
		}
	}

	// 3. 只交付安全的计数和进度标记，供操作人员跟踪本轮处理。
	logger.Info("分片会话收尾轮次结束",
		"scanned_objects", len(summary.Scan.Objects), "scanned_pages", summary.Scan.PagesScanned, "scanned_sessions", scanned,
		"scan_errors", scanErrors, "processed", len(summary.Items), "skipped", summary.Skipped, "confirmed", summary.Confirmed,
		"unconfirmed", summary.Unconfirmed, "failed", summary.Failed, "scan_restarted", summary.ScanRestarted, "done", summary.Done)
}
