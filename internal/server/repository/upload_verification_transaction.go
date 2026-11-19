package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// WithUploadVerificationTransaction 在已提交的读回校验后锁定当前任务和对象。
// action 只使用传入的事务 Store 与上下文执行数据库操作，结果由外层提交确认。
func (store *Store) WithUploadVerificationTransaction(
	ctx context.Context,
	verifiedTask *domain.UploadTask,
	verifiedObject *domain.StoredObject,
	digest domain.UploadContentDigest,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
) error {
	// 1. 用摘要相符的结果进入共同的写回复核流程。
	return store.withUploadVerificationResultTransaction(ctx, verifiedTask, verifiedObject, digest, false, action)
}

// withUploadVerificationResultTransaction 复用对象身份、声明和租约的锁内检查。
func (store *Store) withUploadVerificationResultTransaction(
	ctx context.Context,
	verifiedTask *domain.UploadTask,
	verifiedObject *domain.StoredObject,
	digest domain.UploadContentDigest,
	expectChecksumMismatch bool,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
) error {
	// 1. 检查校验快照和实际摘要，开启用户事务。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核上传校验结果需要数据入口、上下文和事务回调")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if verifiedTask == nil || verifiedTask.ID == 0 || verifiedTask.UserID == 0 || verifiedTask.StagedObjectID == 0 ||
		verifiedTask.LeaseVersion == 0 || verifiedTask.LeaseOwner == nil || *verifiedTask.LeaseOwner == "" ||
		verifiedTask.LeaseUntil == nil || !verifiedTask.LeaseUntil.After(verifiedTask.UpdatedAt) {
		return domain.ErrInvalidUploadTaskData
	}
	if verifiedTask.Status != domain.UploadStatusVerifying || verifiedTask.UploadMode != "SINGLE" {
		return domain.ErrUploadTaskNotReady
	}
	if verifiedTask.FileSize > domain.MaxSingleUploadSizeBytes {
		return domain.ErrUploadFileTooLarge
	}
	if verifiedObject == nil || verifiedObject.ID != verifiedTask.StagedObjectID || verifiedObject.OwnerUserID != verifiedTask.UserID ||
		verifiedObject.Status != domain.StoredObjectStatusVerifying || verifiedObject.FileSize != verifiedTask.FileSize {
		return domain.ErrInvalidStoredObjectData
	}
	if digest.SizeBytes != verifiedTask.FileSize {
		return domain.ErrUploadBodySizeMismatch
	}
	expected := copyUploadTaskValue(verifiedTask.ExpectedSHA256)
	checksumMismatch := expected != nil && digest.SHA256 != *expected
	if expectChecksumMismatch && !checksumMismatch {
		return domain.ErrInvalidUploadTaskData
	}
	if !expectChecksumMismatch && checksumMismatch {
		return domain.ErrUploadChecksumMismatch
	}
	userID, taskID, objectID := verifiedTask.UserID, verifiedTask.ID, verifiedTask.StagedObjectID
	leaseOwner, leaseVersion := *verifiedTask.LeaseOwner, verifiedTask.LeaseVersion
	objectSnapshot := *verifiedObject
	objectSnapshot.StorageVersionID = copyUploadTaskValue(verifiedObject.StorageVersionID)
	objectSnapshot.StorageETag = copyUploadTaskValue(verifiedObject.StorageETag)

	return store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
		// 2. 加锁读取当前任务和对象，比较租约与存储位置。
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		task, err := lockedStore.FindUserUploadTask(txContext, userID, taskID)
		if err != nil {
			return err
		}
		if task.Status != domain.UploadStatusVerifying || task.UploadMode != "SINGLE" {
			return domain.ErrUploadTaskNotReady
		}
		if task.StagedObjectID != objectID || task.LeaseVersion != leaseVersion || task.LeaseOwner == nil ||
			*task.LeaseOwner != leaseOwner || task.LeaseUntil == nil {
			return domain.ErrUploadTaskBusy
		}
		if task.FileSize != digest.SizeBytes || task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil ||
			task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil ||
			(expected == nil) != (task.ExpectedSHA256 == nil) || (expected != nil && *expected != *task.ExpectedSHA256) {
			return domain.ErrInvalidUploadTaskData
		}
		object, err := lockedStore.FindUserStoredObject(txContext, userID, objectID)
		if err != nil {
			return err
		}
		if object.Status != domain.StoredObjectStatusVerifying || object.FileSize != task.FileSize ||
			object.Provider != objectSnapshot.Provider || object.Bucket != objectSnapshot.Bucket || object.ObjectKey != objectSnapshot.ObjectKey ||
			!sameUploadStorageText(object.StorageVersionID, objectSnapshot.StorageVersionID) ||
			!sameUploadStorageText(object.StorageETag, objectSnapshot.StorageETag) ||
			object.VerifiedAt != nil || object.VerifiedSHA256 != nil || object.UnreferencedAt != nil {
			return domain.ErrInvalidStoredObjectData
		}

		// 3. 用数据库时间确认租约有效，再执行事务回调。
		var now time.Time
		queryError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
		if err := txContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return fmt.Errorf("读取校验写回复核时间失败: %w", queryError)
		}
		now = now.UTC()
		if now.Year() < 1000 || now.Year() > 9999 || now.Nanosecond()%1000 != 0 {
			return domain.ErrInvalidUploadTaskData
		}
		if !now.Before(task.ExpiresAt) {
			return domain.ErrUploadTaskNotReady
		}
		if !now.Before(*task.LeaseUntil) {
			return domain.ErrUploadTaskBusy
		}
		return action(txContext, txStore, task, object)
	})
}
