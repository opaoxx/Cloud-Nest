package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
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
	return store.withUploadVerificationResultTransaction(ctx, verifiedTask, verifiedObject, digest.SizeBytes, &digest.SHA256, false, action)
}

// withUploadVerificationResultTransaction 复用对象身份、声明和租约的锁内检查。
func (store *Store) withUploadVerificationResultTransaction(
	ctx context.Context,
	verifiedTask *domain.UploadTask,
	verifiedObject *domain.StoredObject,
	observedSize uint64,
	checksum *[sha256.Size]byte,
	expectChecksumMismatch bool,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
) error {
	// 1. 检查校验快照和读回结果，开启用户事务。
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
	uploadMode := verifiedTask.UploadMode
	// Multipart 复核校验通过和已确认的大小失配两种结果。
	multipart := uploadMode == "MULTIPART" && !expectChecksumMismatch
	if verifiedTask.Status != domain.UploadStatusVerifying || (uploadMode != "SINGLE" && !multipart) {
		return domain.ErrUploadTaskNotReady
	}
	maxSize := domain.MaxSingleUploadSizeBytes
	if multipart {
		maxSize = math.MaxInt64
		if verifiedTask.StorageUploadID == nil || !domain.ValidMultipartUploadID(*verifiedTask.StorageUploadID) ||
			verifiedTask.ExpectedSHA256 == nil || verifiedTask.PartSize == nil || verifiedTask.PartCount == nil ||
			!domain.ValidMultipartUploadLayout(verifiedTask.FileSize, *verifiedTask.PartSize, *verifiedTask.PartCount) ||
			verifiedTask.ReservedBytes != verifiedTask.FileSize {
			return domain.ErrInvalidUploadTaskData
		}
	}
	if verifiedTask.FileSize > maxSize {
		return domain.ErrUploadFileTooLarge
	}
	if verifiedObject == nil || verifiedObject.ID != verifiedTask.StagedObjectID || verifiedObject.OwnerUserID != verifiedTask.UserID ||
		verifiedObject.Status != domain.StoredObjectStatusVerifying || verifiedObject.FileSize != verifiedTask.FileSize {
		return domain.ErrInvalidStoredObjectData
	}
	if multipart && (!validMultipartMergeObjectText(verifiedObject.ObjectKey) || verifiedObject.StorageETag == nil ||
		!validMultipartMergeObjectText(*verifiedObject.StorageETag) ||
		(verifiedObject.StorageVersionID != nil && !validMultipartMergeObjectText(*verifiedObject.StorageVersionID))) {
		return domain.ErrInvalidStoredObjectData
	}
	declaredSize := verifiedTask.FileSize
	expected := copyUploadTaskValue(verifiedTask.ExpectedSHA256)
	if checksum == nil {
		// 大小不符时只需要确认提前 EOF，或者已读到声明范围外的一个字节。
		if observedSize == declaredSize || observedSize > declaredSize+1 {
			return domain.ErrInvalidUploadTaskData
		}
	} else {
		if observedSize != declaredSize {
			return domain.ErrUploadBodySizeMismatch
		}
		checksumMismatch := expected != nil && *checksum != *expected
		if expectChecksumMismatch && !checksumMismatch {
			return domain.ErrInvalidUploadTaskData
		}
		if !expectChecksumMismatch && checksumMismatch {
			return domain.ErrUploadChecksumMismatch
		}
	}
	userID, taskID, objectID := verifiedTask.UserID, verifiedTask.ID, verifiedTask.StagedObjectID
	leaseOwner, leaseVersion := *verifiedTask.LeaseOwner, verifiedTask.LeaseVersion
	taskSnapshot := *verifiedTask
	if multipart {
		taskSnapshot.StorageUploadID = copyUploadTaskValue(verifiedTask.StorageUploadID)
		taskSnapshot.PartSize = copyUploadTaskValue(verifiedTask.PartSize)
		taskSnapshot.PartCount = copyUploadTaskValue(verifiedTask.PartCount)
		taskSnapshot.ExpectedSHA256 = expected
	}
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
		if task.Status != domain.UploadStatusVerifying || task.UploadMode != uploadMode {
			return domain.ErrUploadTaskNotReady
		}
		if task.StagedObjectID != objectID || task.LeaseVersion != leaseVersion || task.LeaseOwner == nil ||
			*task.LeaseOwner != leaseOwner || task.LeaseUntil == nil ||
			(multipart && !sameUploadStorageText(task.StorageUploadID, taskSnapshot.StorageUploadID)) {
			return domain.ErrUploadTaskBusy
		}
		if task.FileSize != declaredSize || task.ReservedBytes != task.FileSize || task.PublishedObjectID != nil ||
			task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil ||
			(expected == nil) != (task.ExpectedSHA256 == nil) || (expected != nil && *expected != *task.ExpectedSHA256) {
			return domain.ErrInvalidUploadTaskData
		}
		object, err := lockedStore.FindUserStoredObject(txContext, userID, objectID)
		if err != nil {
			return err
		}
		if object.Status != domain.StoredObjectStatusVerifying || object.FileSize != task.FileSize ||
			!sameStoredObjectLocation(&objectSnapshot, object) ||
			!sameUploadStorageText(object.StorageETag, objectSnapshot.StorageETag) ||
			object.VerifiedAt != nil || object.VerifiedSHA256 != nil || object.UnreferencedAt != nil ||
			(multipart && (object.GCAfter != nil || object.LastErrorCode != nil)) {
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
		if multipart && !sameMultipartUploadDeclaration(&taskSnapshot, task) {
			return domain.ErrInvalidUploadTaskData
		}
		return action(txContext, txStore, task, object)
	})
}
