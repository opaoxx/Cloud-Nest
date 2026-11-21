package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

const (
	uploadMergeInterval        = 5 * time.Second
	uploadMergeBatchLimit      = 10
	uploadMergeReceiptPageSize = 1000
)

// newServerUploadMergeBatch 绑定同一份存储配置以及已连接的 MySQL 仓储与 MinIO 客户端。
// verifyInstance 由受信部署侧提供，未提供时保留存储缺失任务的重试路径。
func newServerUploadMergeBatch(
	store *repository.Store, storageClient *storage.Client, storageConfig config.StorageConfig, verifyInstance storage.MultipartSessionLockVerifier,
) *service.UploadMergeBatch {
	// 1. 固定仓储读取的原会话和对象，再装配合并前的只读核对。
	targets := service.NewUploadPartSigningTargetService(storageConfig.Provider, storageConfig.Bucket, store.FindUserUploadTaskForPartSigning, store.FindUserStoredObject)
	recovery := service.NewUploadMergeRecoveryService(targets.GetForMerge, storageClient.StatMultipartUploadObject)
	receipts := service.NewUploadMergeReceiptService(targets.GetForMerge, store.ReadMultipartCompletionReceipts,
		func(ctx context.Context, objectKey, uploadID string) ([]domain.UploadPartReceipt, error) {
			return storageClient.ListUploadParts(ctx, objectKey, uploadID, uploadMergeReceiptPageSize)
		})
	preflight := service.NewUploadMergePreflightService(recovery, receipts)

	// 2. 绑定合并调用、校验与补传写回，以及有受信依据的失败收尾。
	execution := service.NewUploadMergeExecutionService(storageConfig.Provider, storageConfig.Bucket,
		preflight.Check, storageClient.CompleteMultipartUpload, store.BeginMultipartUploadVerification, store.ScheduleMultipartUploadMergeRetry,
		preflight.CheckRepair, store.ReopenMultipartUploadForRepair, preflight.CheckMissingSession,
		func(ctx context.Context, objectKey, uploadID string) (bool, error) {
			return storageClient.ConfirmMultipartSessionAbsence(ctx, objectKey, uploadID, verifyInstance)
		}, store.FailMultipartUploadForMissingSession)

	// 3. 接入候选扫描和领租，返回可独立执行的一轮。
	return service.NewUploadMergeBatch(store.ListMultipartUploadCompletionCandidates, store.ClaimMultipartUploadCompletionLease, execution.Execute)
}

// startServerUploadMergeWorker 启动合并循环，返回取消并等待实际退出的方法。
func startServerUploadMergeWorker(
	ctx context.Context, runOnce func(context.Context, int, string) (service.UploadMergeBatchResult, error), logger *slog.Logger,
) func() {
	// 1. 派生服务停止信号，为本次运行生成独立的租约持有者。
	workerContext, cancel := context.WithCancel(ctx)
	leaseOwner := "multipart-merge-" + uuid.NewString()
	done := make(chan struct{})

	// 2. 串行处理并汇报合并批次，错误日志使用已有的固定错误码。
	logger.Info("分片合并 worker 已启动", "interval_ms", uploadMergeInterval.Milliseconds(), "batch_limit", uploadMergeBatchLimit)
	go func() {
		defer close(done)
		defer logger.Info("分片合并 worker 已停止")
		err := service.RunUploadMergeLoop(workerContext, uploadMergeInterval,
			func(roundContext context.Context) (service.UploadMergeBatchResult, error) {
				return runOnce(roundContext, uploadMergeBatchLimit, leaseOwner)
			},
			func(summary service.UploadMergeBatchResult, roundError error) {
				stopError := workerContext.Err()
				if roundError != nil && (stopError == nil || !errors.Is(roundError, stopError)) {
					logger.Warn("分片合并轮次失败", "error_code", uploadWorkerErrorCode(roundError))
				}
				for _, failure := range summary.CandidateErrors {
					if stopError != nil && errors.Is(failure.Err, stopError) {
						continue
					}
					logger.Warn("分片合并任务处理失败", "task_id", failure.TaskID, "error_code", uploadWorkerErrorCode(failure.Err))
				}
				if summary.Scanned > 0 {
					logger.Info("分片合并轮次结束", "scanned", summary.Scanned, "verifying", summary.Verifying,
						"reopened", summary.Reopened, "failed", summary.Failed, "retries_scheduled", summary.RetriesScheduled,
						"skipped", summary.Skipped, "candidate_errors", len(summary.CandidateErrors))
				}
			})
		if err != nil && workerContext.Err() == nil {
			logger.Error("分片合并 worker 异常退出", "error_code", uploadWorkerErrorCode(err))
		}
	}()

	// 3. 取消后等待最后一轮汇报及退出日志完成。
	return func() {
		cancel()
		<-done
	}
}
