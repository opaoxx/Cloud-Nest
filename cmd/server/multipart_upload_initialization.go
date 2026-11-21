package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

const (
	multipartUploadInitializationInterval   = 5 * time.Second
	multipartUploadInitializationBatchLimit = 10
)

// newServerMultipartUploadInitializationBatch 使用已初始化的 MySQL 仓储及 MinIO 客户端。
// storageConfig 是创建存储客户端时使用的配置。
func newServerMultipartUploadInitializationBatch(store *repository.Store, storageClient *storage.Client, storageConfig config.StorageConfig) *service.MultipartUploadInitializationBatch {
	// 1. 绑定单条领租、对象读取、存储创建及两种结果写回。
	processor := service.NewMultipartUploadInitializationProcessor(
		storageConfig.Provider, storageConfig.Bucket,
		store.ClaimMultipartUploadInitializationLease, store.FindUserStoredObject,
		storageClient.CreateMultipartUpload, store.BindMultipartUploadSession, store.ScheduleMultipartUploadInitializationRetry,
	)
	// 2. 绑定候选扫描，提供可直接调用的单轮入口。
	return service.NewMultipartUploadInitializationBatch(store.ListMultipartUploadInitializationCandidates, processor.Process)
}

// startServerMultipartUploadInitializationWorker 启动初始化循环并返回取消且等待退出的方法。
func startServerMultipartUploadInitializationWorker(
	ctx context.Context,
	runOnce func(context.Context, int, string) (service.MultipartUploadInitializationBatchResult, error),
	logger *slog.Logger,
) func() {
	// 1. 从服务上下文派生停止信号，为本次运行生成独立的租约持有者。
	workerContext, cancel := context.WithCancel(ctx)
	leaseOwner := "multipart-init-" + uuid.NewString()
	done := make(chan struct{})

	// 2. 串行处理并汇报初始化批次，日志只使用固定错误码。
	logger.Info("分片上传初始化 worker 已启动", "interval_ms", multipartUploadInitializationInterval.Milliseconds(),
		"batch_limit", multipartUploadInitializationBatchLimit)
	go func() {
		defer close(done)
		defer logger.Info("分片上传初始化 worker 已停止")
		err := service.RunMultipartUploadInitializationLoop(workerContext, multipartUploadInitializationInterval,
			func(roundContext context.Context) (service.MultipartUploadInitializationBatchResult, error) {
				return runOnce(roundContext, multipartUploadInitializationBatchLimit, leaseOwner)
			},
			func(summary service.MultipartUploadInitializationBatchResult, roundError error) {
				stopError := workerContext.Err()
				if roundError != nil && (stopError == nil || !errors.Is(roundError, stopError)) {
					logger.Warn("分片上传初始化轮次失败", "error_code", uploadWorkerErrorCode(roundError))
				}
				for _, failure := range summary.CandidateErrors {
					if stopError != nil && errors.Is(failure.Err, stopError) {
						continue
					}
					logger.Warn("分片上传初始化任务处理失败", "task_id", failure.TaskID, "error_code", uploadWorkerErrorCode(failure.Err))
				}
				if summary.Scanned > 0 {
					logger.Info("分片上传初始化轮次结束", "scanned", summary.Scanned, "bound", summary.Bound,
						"retries_scheduled", summary.RetriesScheduled, "candidate_errors", len(summary.CandidateErrors))
				}
			})
		if err != nil && workerContext.Err() == nil {
			logger.Error("分片上传初始化 worker 异常退出", "error_code", uploadWorkerErrorCode(err))
		}
	}()

	// 3. 取消后等待在途批次汇报完毕，直到 worker 实际退出。
	return func() {
		cancel()
		<-done
	}
}
