package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadCancellationExecutionService 处理一条已领取租约的 Multipart 取消任务。
type MultipartUploadCancellationExecutionService struct {
	provider, bucket                  string
	observeBound, observeUnbound      func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCancellationObservation, error)
	completeMissing, completeExisting func(context.Context, string, string, *domain.MultipartUploadCancellationObservation) (*domain.UploadTask, error)
}

// NewMultipartUploadCancellationExecutionService 绑定同一存储配置的观察及终态事务。
func NewMultipartUploadCancellationExecutionService(
	provider, bucket string,
	observeBound, observeUnbound func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCancellationObservation, error),
	completeMissing, completeExisting func(context.Context, string, string, *domain.MultipartUploadCancellationObservation) (*domain.UploadTask, error),
) *MultipartUploadCancellationExecutionService {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || observeBound == nil || observeUnbound == nil || completeMissing == nil || completeExisting == nil {
		panic("单条分片取消执行需要存储配置及两种观察和终态事务")
	}
	return &MultipartUploadCancellationExecutionService{provider: provider, bucket: bucket,
		observeBound: observeBound, observeUnbound: observeUnbound, completeMissing: completeMissing, completeExisting: completeExisting}
}

// Execute 按当前租约处理一次，返回已提交的 CANCELLED 任务。nil, nil 表示存储待确认。
func (execution *MultipartUploadCancellationExecutionService) Execute(ctx context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
	// 1. 按原会话绑定选择观察方式。
	if err := validateCancellationStorageClaim(ctx, claimed); err != nil {
		return nil, err
	}
	observe := execution.observeUnbound
	if claimed.StorageUploadID != nil {
		if !domain.ValidMultipartUploadID(*claimed.StorageUploadID) {
			return nil, domain.ErrInvalidUploadTaskData
		}
		observe = execution.observeBound
	}
	observation, err := observe(ctx, claimed)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if observation == nil {
		return nil, nil
	}

	// 2. 核对观察仍属于本次租约，再调用对应的终态事务。
	observed := &observation.Task
	if observation.ObjectKey == "" || !sameMultipartCancellationTaskBinding(observed, claimed) ||
		observed.Status != claimed.Status || observed.ReservedBytes != claimed.ReservedBytes ||
		observed.LeaseOwner == nil || *observed.LeaseOwner != *claimed.LeaseOwner ||
		observed.LeaseUntil == nil || !observed.LeaseUntil.Equal(*claimed.LeaseUntil) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	complete := execution.completeMissing
	if observation.Object != nil {
		complete = execution.completeExisting
	}
	finished, err := complete(ctx, execution.provider, execution.bucket, observation)
	if err != nil {
		return nil, err
	}

	// 3. 确认返回的是同一任务已提交的 CANCELLED 结果。
	if finished == nil || !sameMultipartCancellationTaskBinding(finished, claimed) ||
		finished.Status != domain.UploadStatusCancelled || finished.ReservedBytes != 0 ||
		finished.LeaseOwner != nil || finished.LeaseUntil != nil || finished.NextAttemptAt != nil ||
		finished.PublishedObjectID != nil || finished.ResultEntryID != nil || finished.ResolvedName != nil ||
		finished.LastErrorCode != nil || finished.LastErrorMessage != nil ||
		finished.CompletedAt == nil || !finished.CompletedAt.Equal(finished.UpdatedAt) ||
		finished.CompletedAt.Before(claimed.UpdatedAt) || !finished.CompletedAt.Before(*claimed.LeaseUntil) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return finished, nil
}

func sameMultipartCancellationTaskBinding(current, claimed *domain.UploadTask) bool {
	return current.ID == claimed.ID && current.UserID == claimed.UserID && current.StagedObjectID == claimed.StagedObjectID &&
		current.UploadMode == claimed.UploadMode && current.LeaseVersion == claimed.LeaseVersion && current.AttemptCount == claimed.AttemptCount &&
		current.FileSize == claimed.FileSize && current.ExpiresAt.Equal(claimed.ExpiresAt) &&
		sameMultipartCancellationSession(current.StorageUploadID, claimed.StorageUploadID)
}
