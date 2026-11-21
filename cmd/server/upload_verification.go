package main

import (
	"context"
	"errors"
	"io"
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
	// 1. 绑定 SINGLE 的候选扫描与租约领取。
	processor := newServerUploadVerificationProcessor(store, store.ClaimUploadVerificationLease, storageClient.OpenUploadVerificationObject, nil)
	return service.NewUploadVerificationBatch(store.ListUploadVerificationCandidates, processor.Process)
}

// newServerMultipartUploadVerificationBatch 装配一轮有上限的分片校验。
func newServerMultipartUploadVerificationBatch(store *repository.Store, open func(context.Context, *domain.StoredObject) (io.ReadCloser, error)) *service.UploadVerificationBatch {
	// 1. 绑定 Multipart 扫描和单条处理，由共用批次顺序执行并汇总。
	processor := newServerMultipartUploadVerificationProcessor(store, open)
	return service.NewUploadVerificationBatch(store.ListMultipartUploadVerificationCandidates, processor.Process)
}

// newServerMultipartUploadVerificationProcessor 装配一条已合并任务的校验入口。
func newServerMultipartUploadVerificationProcessor(store *repository.Store, open func(context.Context, *domain.StoredObject) (io.ReadCloser, error)) *service.UploadVerificationCandidateProcessor {
	// 1. Multipart 绑定领取与续租，读回及写回复用现有校验流程。
	renew := func(ctx context.Context, task *domain.UploadTask) error {
		_, err := store.RenewMultipartUploadVerificationLease(ctx, task)
		return err
	}
	return newServerUploadVerificationProcessor(store, store.ClaimMultipartUploadVerificationLease, open, renew)
}

func newServerUploadVerificationProcessor(
	store *repository.Store,
	claimLease func(context.Context, domain.UploadVerificationCandidate, string) (*domain.UploadTask, error),
	open func(context.Context, *domain.StoredObject) (io.ReadCloser, error),
	renew func(context.Context, *domain.UploadTask) error,
) *service.UploadVerificationCandidateProcessor {
	// 1. 将发布服务接到 MySQL 的校验写回事务。
	publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest,
		publish func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
		return store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *repository.Store, currentTask *domain.UploadTask, currentObject *domain.StoredObject) error {
			return publish(txContext, txStore, currentTask, currentObject)
		})
	})

	// 2. 绑定按原版本读回、失败收尾和重试写回。
	verification := service.NewUploadVerificationService(
		open,
		publisher.Publish,
		store.FailUploadForChecksumMismatch,
		store.FailUploadForBodySizeMismatch,
		store.ScheduleUploadVerificationRetry,
	)

	// 3. 领取提交后按账号读取对象，再执行校验与结果写回。
	verify := verification.VerifyAndPublish
	if renew != nil {
		verify = func(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject) (*domain.UploadTask, error) {
			return verification.VerifyMultipartAndPublish(ctx, task, staged, renew)
		}
	}
	return service.NewUploadVerificationCandidateProcessor(claimLease, store.FindUserStoredObject, verify)
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
				stopError := workerContext.Err()
				if roundError != nil && stopError == nil {
					logger.Warn("上传校验轮次失败")
				}
				// 1. 遍历本轮逐条错误，识别服务停止造成的取消。
				for _, failure := range summary.CandidateErrors {
					if stopError != nil && errors.Is(failure.Err, stopError) {
						continue
					}
					// 2. 根据错误类型和可识别的原因选取固定错误码。
					code := uploadVerificationErrorCode(failure.Err)
					// 3. 只将任务 ID 和错误码写入结构化日志。
					logger.Warn("上传校验任务处理失败", "task_id", failure.TaskID, "error_code", code)
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

// uploadVerificationErrorCode 为后台校验日志返回固定错误码。
func uploadVerificationErrorCode(err error) string {
	// 1. 识别校验服务交回的原始、已确认的失配证据。
	switch mismatch := err.(type) {
	case *domain.UploadVerificationSizeMismatch:
		if mismatch != nil {
			return domain.UploadBodySizeMismatchCode
		}
	case *domain.UploadVerificationChecksumMismatch:
		if mismatch != nil {
			return domain.UploadChecksumMismatchCode
		}
	}

	// 2. 其余原因交给上传 worker 共用的错误码映射。
	return uploadWorkerErrorCode(err)
}
