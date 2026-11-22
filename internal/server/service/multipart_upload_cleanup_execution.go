package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type multipartUploadCleanupExecution struct {
	provider, bucket                  string
	observeBound, observeUnbound      func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCleanupObservation, error)
	completeMissing, completeExisting func(context.Context, string, string, *domain.MultipartUploadCleanupObservation) (*domain.UploadTask, error)
}

func (cleanup multipartUploadCleanupExecution) observe(
	ctx context.Context, claimed *domain.UploadTask, pendingStatus domain.UploadStatus,
) (*domain.MultipartUploadCleanupObservation, error) {
	// 1. 按原会话是否绑定，选择对应的存储复查。
	if err := validateMultipartCleanupStorageClaim(ctx, claimed, pendingStatus); err != nil {
		return nil, err
	}
	observe := cleanup.observeUnbound
	if claimed.StorageUploadID != nil {
		if !domain.ValidMultipartUploadID(*claimed.StorageUploadID) {
			return nil, domain.ErrInvalidUploadTaskData
		}
		observe = cleanup.observeBound
	}
	observation, err := observe(ctx, claimed)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return observation, nil
}

func (cleanup multipartUploadCleanupExecution) complete(
	ctx context.Context, claimed *domain.UploadTask, observation *domain.MultipartUploadCleanupObservation, terminalStatus domain.UploadStatus,
) (*domain.UploadTask, error) {
	// 2. 核对观察仍属于本次租约，再选择终态事务。
	observed := &observation.Task
	if observation.ObjectKey == "" || !sameMultipartCleanupTaskBinding(observed, claimed) ||
		observed.Status != claimed.Status || observed.ReservedBytes != claimed.ReservedBytes || observed.AttemptCount != claimed.AttemptCount ||
		observed.LeaseOwner == nil || *observed.LeaseOwner != *claimed.LeaseOwner ||
		observed.LeaseUntil == nil || !observed.LeaseUntil.Equal(*claimed.LeaseUntil) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	complete := cleanup.completeMissing
	if observation.Object != nil {
		complete = cleanup.completeExisting
	}
	finished, err := complete(ctx, cleanup.provider, cleanup.bucket, observation)
	if err != nil {
		return nil, err
	}
	// 3. 核对仓储提交的是同一任务的终态结果。
	if !validMultipartCleanupCompletion(finished, claimed, terminalStatus) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return finished, nil
}

func validMultipartCleanupCompletion(finished, claimed *domain.UploadTask, terminalStatus domain.UploadStatus) bool {
	return finished != nil && sameMultipartCleanupTaskBinding(finished, claimed) &&
		finished.Status == terminalStatus && finished.ReservedBytes == 0 && finished.AttemptCount == claimed.AttemptCount &&
		finished.LeaseOwner == nil && finished.LeaseUntil == nil && finished.NextAttemptAt == nil &&
		finished.PublishedObjectID == nil && finished.ResultEntryID == nil && finished.ResolvedName == nil &&
		finished.LastErrorCode == nil && finished.LastErrorMessage == nil &&
		finished.CompletedAt != nil && finished.CompletedAt.Equal(finished.UpdatedAt) &&
		!finished.CompletedAt.Before(claimed.UpdatedAt) && finished.CompletedAt.Before(*claimed.LeaseUntil) &&
		(terminalStatus != domain.UploadStatusExpired || !finished.CompletedAt.Before(claimed.ExpiresAt))
}

func sameMultipartCleanupTaskBinding(current, claimed *domain.UploadTask) bool {
	return current.ID == claimed.ID && current.UserID == claimed.UserID && current.StagedObjectID == claimed.StagedObjectID &&
		current.UploadMode == claimed.UploadMode && current.LeaseVersion == claimed.LeaseVersion &&
		current.FileSize == claimed.FileSize && current.ExpiresAt.Equal(claimed.ExpiresAt) &&
		sameMultipartCleanupSession(current.StorageUploadID, claimed.StorageUploadID)
}
