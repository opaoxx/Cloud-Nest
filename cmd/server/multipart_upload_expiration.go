package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

// newServerMultipartUploadExpirationBatch 装配匹配同一存储配置的到期批次。
// 已绑定任务缺少受信实例核对时继续持有预占并登记重试。
func newServerMultipartUploadExpirationBatch(
	store *repository.Store, storageClient *storage.Client, storageConfig config.StorageConfig, verifyInstance storage.MultipartSessionLockVerifier,
) *service.MultipartUploadExpirationBatch {
	// 1. 绑定原对象定位、会话中止与存储复查。
	observations := service.NewMultipartUploadExpirationStorageService(
		func(ctx context.Context, task *domain.UploadTask) (*domain.MultipartUploadCleanupTarget, error) {
			return store.FindMultipartUploadExpirationTarget(ctx, task, storageConfig.Provider, storageConfig.Bucket)
		}, storageClient.AbortMultipartUpload,
		func(ctx context.Context, objectKey, uploadID string) (*domain.MultipartUploadAbortObservation, error) {
			return storageClient.CheckMultipartUploadAfterAbort(ctx, objectKey, uploadID, verifyInstance)
		}, storageClient.StatMultipartUploadObject)

	// 2. 绑定两种到期终态事务和重试登记。
	execution := service.NewMultipartUploadExpirationExecutionService(storageConfig.Provider, storageConfig.Bucket,
		observations.ObserveBoundSession, observations.ObserveUnboundSession,
		store.CompleteMissingMultipartUploadExpiration, store.CompleteExistingMultipartUploadExpiration,
		store.ScheduleMultipartUploadExpirationRetry)

	// 3. 接入候选扫描与租约领取，返回单轮批次。
	return service.NewMultipartUploadExpirationBatch(store.ListMultipartUploadExpirationCandidates,
		store.ClaimMultipartUploadExpirationLease, execution.Execute)
}

// startServerMultipartUploadExpirationWorker 启动到期循环并返回取消且等待退出的方法。
func startServerMultipartUploadExpirationWorker(
	ctx context.Context,
	runOnce func(context.Context, int, string) (service.MultipartUploadExpirationBatchResult, error),
	logger *slog.Logger,
) func() {
	// 1. 派生停止上下文，为本次运行生成租约持有者标识。
	workerContext, cancel := context.WithCancel(ctx)
	leaseOwner := "multipart-expire-" + uuid.NewString()
	done := make(chan struct{})

	// 2. 执行到期循环，按固定字段汇报每轮结果。
	logger.Info("分片上传到期 worker 已启动", "interval_ms", uploadExpirationInterval.Milliseconds(),
		"batch_limit", domain.MaxUploadExpirationCandidates)
	go func() {
		defer close(done)
		defer logger.Info("分片上传到期 worker 已停止")
		err := service.RunMultipartUploadExpirationLoop(workerContext, uploadExpirationInterval,
			func(roundContext context.Context) (service.MultipartUploadExpirationBatchResult, error) {
				return runOnce(roundContext, domain.MaxUploadExpirationCandidates, leaseOwner)
			},
			func(summary service.MultipartUploadExpirationBatchResult, roundError error) {
				reportMultipartUploadExpirationRound(logger, summary, roundError, workerContext.Err())
			})
		if code := multipartUploadCleanupReportCode(err, workerContext.Err()); code != "" {
			logger.Error("分片上传到期 worker 异常退出", "error_code", code)
		}
	}()

	// 3. 停止时取消在途轮次，等汇报和退出日志写完再返回。
	return func() {
		cancel()
		<-done
	}
}

func reportMultipartUploadExpirationRound(
	logger *slog.Logger, summary service.MultipartUploadExpirationBatchResult, roundError, stopError error,
) {
	// 1. 用固定错误码汇报整轮和逐项故障，过滤正常关停信号。
	if code := multipartUploadCleanupReportCode(roundError, stopError); code != "" {
		logger.Warn("分片上传到期轮次失败", "error_code", code)
	}
	for _, failure := range summary.CandidateErrors {
		if code := multipartUploadCleanupReportCode(failure.Err, stopError); code != "" {
			logger.Warn("分片上传到期任务处理失败", "task_id", failure.TaskID, "error_code", code)
		}
	}

	// 2. 汇报本轮的扫描、终态、重试、跳过和逐项错误计数。
	if summary.Scanned > 0 {
		logger.Info("分片上传到期轮次结束", "scanned", summary.Scanned, "expired", summary.Expired,
			"retries_scheduled", summary.RetriesScheduled, "skipped", summary.Skipped, "candidate_errors", len(summary.CandidateErrors))
	}
}
