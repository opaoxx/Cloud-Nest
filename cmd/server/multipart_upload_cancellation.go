package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

// newServerMultipartUploadCancellationBatch 使用已连接的仓储、存储及对应配置装配取消批次。
// verifyInstance 由受信部署侧提供；缺少确认时，已绑定任务保留预占并登记重试。
func newServerMultipartUploadCancellationBatch(
	store *repository.Store, storageClient *storage.Client, storageConfig config.StorageConfig, verifyInstance storage.MultipartSessionLockVerifier,
) *service.MultipartUploadCancellationBatch {
	// 1. 绑定原存储目标、中止和中止后的复查。
	observations := service.NewMultipartUploadCancellationStorageService(
		func(ctx context.Context, task *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error) {
			return store.FindMultipartUploadCancellationTarget(ctx, task, storageConfig.Provider, storageConfig.Bucket)
		}, storageClient.AbortMultipartUpload,
		func(ctx context.Context, objectKey, uploadID string) (*domain.MultipartUploadAbortObservation, error) {
			return storageClient.CheckMultipartUploadAfterAbort(ctx, objectKey, uploadID, verifyInstance)
		}, storageClient.StatMultipartUploadObject)

	// 2. 绑定两种确认结果的事务写回，以及待确认时的重试登记。
	execution := service.NewMultipartUploadCancellationExecutionService(storageConfig.Provider, storageConfig.Bucket,
		observations.ObserveBoundSession, observations.ObserveUnboundSession,
		store.CompleteMissingMultipartUploadCancellation, store.CompleteExistingMultipartUploadCancellation,
		store.ScheduleMultipartUploadCancellationRetry)

	// 3. 接入候选扫描和租约领取，返回单轮批次。
	return service.NewMultipartUploadCancellationBatch(store.ListMultipartUploadCancellationCandidates,
		store.ClaimMultipartUploadCancellationLease, execution.Execute)
}

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
