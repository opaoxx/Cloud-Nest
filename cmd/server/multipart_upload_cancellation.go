package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// startServerMultipartUploadCancellationWorker 启动分片取消循环并返回取消且等待退出的方法。
func startServerMultipartUploadCancellationWorker(
	ctx context.Context,
	runOnce func(context.Context, int, string) (service.MultipartUploadCancellationBatchResult, error),
	logger *slog.Logger,
) func() {
	// 1. 派生停止信号，为本次运行生成独立的租约持有者。
	workerContext, cancel := context.WithCancel(ctx)
	leaseOwner := "multipart-cancel-" + uuid.NewString()
	done := make(chan struct{})

	// 2. 调用已有定时循环，按固定字段汇报轮次结果。
	logger.Info("分片上传取消 worker 已启动", "interval_ms", uploadCancellationInterval.Milliseconds(),
		"batch_limit", domain.MaxUploadCancellationCandidates)
	go func() {
		defer close(done)
		defer logger.Info("分片上传取消 worker 已停止")
		err := service.RunMultipartUploadCancellationLoop(workerContext, uploadCancellationInterval,
			func(roundContext context.Context) (service.MultipartUploadCancellationBatchResult, error) {
				return runOnce(roundContext, domain.MaxUploadCancellationCandidates, leaseOwner)
			},
			func(summary service.MultipartUploadCancellationBatchResult, roundError error) {
				reportMultipartUploadCancellationRound(logger, summary, roundError, workerContext.Err())
			})
		if code := multipartUploadCancellationReportCode(err, workerContext.Err()); code != "" {
			logger.Error("分片上传取消 worker 异常退出", "error_code", code)
		}
	}()

	// 3. 停止时通知在途轮次，等汇报和退出日志都写完再返回。
	return func() {
		cancel()
		<-done
	}
}

func reportMultipartUploadCancellationRound(
	logger *slog.Logger, summary service.MultipartUploadCancellationBatchResult, roundError, stopError error,
) {
	// 1. 用固定错误码交付整轮错误和逐项故障，省略正常关停信号。
	if code := multipartUploadCancellationReportCode(roundError, stopError); code != "" {
		logger.Warn("分片上传取消轮次失败", "error_code", code)
	}
	for _, failure := range summary.CandidateErrors {
		if code := multipartUploadCancellationReportCode(failure.Err, stopError); code != "" {
			logger.Warn("分片上传取消任务处理失败", "task_id", failure.TaskID, "error_code", code)
		}
	}

	// 2. 按批次口径汇报扫描、取消、重试、跳过及错误总数。
	if summary.Scanned > 0 {
		logger.Info("分片上传取消轮次结束", "scanned", summary.Scanned, "cancelled", summary.Cancelled,
			"retries_scheduled", summary.RetriesScheduled, "skipped", summary.Skipped, "candidate_errors", len(summary.CandidateErrors))
	}
}

func multipartUploadCancellationReportCode(err, stopError error) string {
	// 1. 沿单一包装链识别纯关停信号，保留含其他原因的合并错误。
	if err == nil {
		return ""
	}
	cause := err
	for next := errors.Unwrap(cause); next != nil; next = errors.Unwrap(cause) {
		cause = next
	}
	if cause == stopError {
		return ""
	}

	// 2. 复用取消错误码；合并错误若只映射出停止码，则用通用故障码汇报。
	code := uploadCleanupErrorCode(err)
	if stopError != nil && errors.Is(err, stopError) && code == uploadCleanupErrorCode(stopError) {
		return "INTERNAL_ERROR"
	}
	return code
}
