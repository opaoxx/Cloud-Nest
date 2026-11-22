package repository

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CompleteMissingMultipartUploadExpiration 提交原对象明确缺失的分片到期终态。
// 同一租约的重复交付复用已提交结果。临时数据库错误附带到期重试标记。
func (store *Store) CompleteMissingMultipartUploadExpiration(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCleanupObservation,
) (*domain.UploadTask, error) {
	// 1. 确认调用方已观察到原对象缺失。
	if observation == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if observation.Object != nil {
		return nil, domain.ErrUploadTaskNotReady
	}
	// 2. 在同一事务中复核、返还预占并登记回收，提交后返回终态。
	return store.completeMultipartUploadExpiration(ctx, provider, bucket, observation)
}

func (store *Store) completeMultipartUploadExpiration(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCleanupObservation,
) (*domain.UploadTask, error) {
	completed, err := store.completeMultipartUploadCleanup(ctx, provider, bucket, observation,
		domain.UploadStatusExpiring, domain.UploadStatusExpired)
	return completed, markUploadTransactionRetryError(ctx, err, domain.ErrUploadExpirationRetryable)
}
