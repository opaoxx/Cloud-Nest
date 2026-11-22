package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadExpirationExecutionService 确认一条已领取租约的 Multipart 到期任务。
type MultipartUploadExpirationExecutionService struct {
	cleanup multipartUploadCleanupExecution
}

// NewMultipartUploadExpirationExecutionService 绑定同一存储配置的观察和终态事务。
func NewMultipartUploadExpirationExecutionService(
	provider, bucket string,
	observeBound, observeUnbound func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCleanupObservation, error),
	completeMissing, completeExisting func(context.Context, string, string, *domain.MultipartUploadCleanupObservation) (*domain.UploadTask, error),
) *MultipartUploadExpirationExecutionService {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || observeBound == nil || observeUnbound == nil || completeMissing == nil || completeExisting == nil {
		panic("单条分片到期执行需要存储配置、观察及终态事务")
	}
	return &MultipartUploadExpirationExecutionService{cleanup: multipartUploadCleanupExecution{
		provider: provider, bucket: bucket, observeBound: observeBound, observeUnbound: observeUnbound,
		completeMissing: completeMissing, completeExisting: completeExisting,
	}}
}

// Execute 按当前租约处理一次。返回 EXPIRED 表示仓储已经提交，nil, nil 表示存储仍待确认。
func (execution *MultipartUploadExpirationExecutionService) Execute(ctx context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 根据原会话复查存储，保留待确认结果和原错误。
	observation, err := execution.cleanup.observe(ctx, claimed, domain.UploadStatusExpiring)
	if err != nil || observation == nil {
		return nil, err
	}
	// 2. 交付已确认的观察，核对同一任务的终态已提交。
	return execution.cleanup.complete(ctx, claimed, observation, domain.UploadStatusExpired)
}
