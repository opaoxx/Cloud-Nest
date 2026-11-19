package main

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
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
