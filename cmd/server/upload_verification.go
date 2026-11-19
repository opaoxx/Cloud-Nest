package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

const (
	uploadVerificationInterval   = 5 * time.Second
	uploadVerificationBatchLimit = 10
)

// newServerUploadVerificationBatch 复用服务持有的 MySQL 仓储和 MinIO 客户端。
func newServerUploadVerificationBatch(store *repository.Store, storageClient *storage.Client) *service.UploadVerificationBatch {
	// 1. 将发布服务接到 MySQL 的校验写回事务。
	publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest,
		publish func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
		return store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *repository.Store, currentTask *domain.UploadTask, currentObject *domain.StoredObject) error {
			return publish(txContext, txStore, currentTask, currentObject)
		})
	})

	// 2. 绑定 MinIO 读回、失败收尾和重试写回。
	verification := service.NewUploadVerificationService(
		storageClient.OpenUploadVerificationObject,
		publisher.Publish,
		store.FailUploadForChecksumMismatch,
		store.FailUploadForBodySizeMismatch,
		store.ScheduleUploadVerificationRetry,
	)

	// 3. 接入租约领取、暂存对象查询和候选扫描，返回单轮入口。
	processor := service.NewUploadVerificationCandidateProcessor(store.ClaimUploadVerificationLease, store.FindUserStoredObject, verification.VerifyAndPublish)
	return service.NewUploadVerificationBatch(store.ListUploadVerificationCandidates, processor.Process)
}

// startServerUploadVerificationWorker 启动一个校验循环，返回取消并等待的方法。
func startServerUploadVerificationWorker(ctx context.Context, runOnce func(context.Context, int, string) (service.UploadVerificationBatchResult, error), logger *slog.Logger) func() {
	// 1. 创建随服务取消的上下文，为本次运行生成租约持有者标识。
	workerContext, cancel := context.WithCancel(ctx)
	leaseOwner := "upload-verification-" + uuid.NewString()
	done := make(chan struct{})

	// 2. 启动一个后台循环，执行并汇报每轮校验。
	logger.Info("上传校验 worker 已启动", "interval_ms", uploadVerificationInterval.Milliseconds(), "batch_limit", uploadVerificationBatchLimit)
	go func() {
		defer close(done)
		defer logger.Info("上传校验 worker 已停止")
		err := service.RunUploadVerificationLoop(workerContext, uploadVerificationInterval,
			func(roundContext context.Context) (service.UploadVerificationBatchResult, error) {
				return runOnce(roundContext, uploadVerificationBatchLimit, leaseOwner)
			},
			func(summary service.UploadVerificationBatchResult, roundError error) {
				if roundError != nil && workerContext.Err() == nil {
					logger.Warn("上传校验轮次失败")
				}
				if summary.Scanned > 0 {
					logger.Info("上传校验轮次结束", "scanned", summary.Scanned, "published", summary.Published,
						"retries_scheduled", summary.RetriesScheduled, "candidate_errors", len(summary.CandidateErrors))
				}
			})
		if err != nil && workerContext.Err() == nil {
			logger.Error("上传校验 worker 异常退出", "error", err)
		}
	}()

	// 3. 返回停止方法，取消循环并等待它实际退出。
	return func() {
		cancel()
		<-done
	}
}
