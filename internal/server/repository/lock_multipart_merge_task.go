package repository

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

func validMultipartMergeClaim(claimed *domain.UploadTask) bool {
	return validMultipartCompletionLeaseSnapshot(claimed, domain.UploadStatusCompleting) && claimed.ExpectedSHA256 != nil &&
		claimed.PartSize != nil && claimed.PartCount != nil && claimed.ReservedBytes == claimed.FileSize &&
		domain.ValidMultipartUploadLayout(claimed.FileSize, *claimed.PartSize, *claimed.PartCount)
}

// lockMultipartMergeTask 供已锁定账号的事务复用。调用方已检查领取结果的字段。
func (store *Store) lockMultipartMergeTask(ctx context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.FindUserUploadTask(ctx, claimed.UserID, claimed.ID)
	if err != nil {
		return nil, err
	}
	if current.Status != domain.UploadStatusCompleting || current.UploadMode != "MULTIPART" {
		return nil, domain.ErrUploadTaskNotReady
	}
	if current.StagedObjectID != claimed.StagedObjectID || !sameUploadStorageText(current.StorageUploadID, claimed.StorageUploadID) ||
		current.LeaseVersion != claimed.LeaseVersion || !sameUploadStorageText(current.LeaseOwner, claimed.LeaseOwner) || current.LeaseUntil == nil {
		return nil, domain.ErrUploadTaskBusy
	}
	if err := store.checkMultipartCompletionDeadlines(ctx, current); err != nil {
		return nil, err
	}
	if !sameMultipartUploadDeclaration(claimed, current) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return current, nil
}
