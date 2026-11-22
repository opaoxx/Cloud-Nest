package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// WithMultipartUploadCancellationObservationTransaction 锁定并复核取消租约及其存储观察。
// 回调只用传入的事务上下文和 Store 执行数据库操作，存储访问留在事务外。
func (store *Store) WithMultipartUploadCancellationObservationTransaction(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCancellationObservation,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
) error {
	return store.withMultipartUploadCancellationObservationTransaction(ctx, provider, bucket, observation, action, nil)
}

// WithMultipartUploadExpirationObservationTransaction 锁定并复核到期租约及其存储观察。
// 回调使用事务内的任务和暂存对象。存储访问在事务外完成。
func (store *Store) WithMultipartUploadExpirationObservationTransaction(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCleanupObservation,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
) error {
	return store.withMultipartUploadCleanupObservationTransaction(ctx, provider, bucket, observation, domain.UploadStatusExpiring, action, nil)
}

func (store *Store) withMultipartUploadCancellationObservationTransaction(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCancellationObservation,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
	replay func(*domain.UploadTask, *domain.StoredObject) error,
) error {
	return store.withMultipartUploadCleanupObservationTransaction(ctx, provider, bucket, observation, domain.UploadStatusCancelling, action, replay)
}

func (store *Store) withMultipartUploadCleanupObservationTransaction(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCleanupObservation, pendingStatus domain.UploadStatus,
	action func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error,
	replayCancellation func(*domain.UploadTask, *domain.StoredObject) error,
) error {
	// 1. 校验观察，按归属锁定账号、任务和暂存对象。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核分片收尾观察需要数据入口、上下文和事务回调")
	}
	if observation == nil || !validMultipartUploadCleanupClaim(&observation.Task, pendingStatus) {
		return domain.ErrInvalidUploadTaskData
	}
	claimed := &observation.Task
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: observation.ObjectKey}
	observed := observation.Object
	if !location.IsValid() || !validMultipartMergeObjectText(location.ObjectKey) ||
		(observed != nil && (observed.StorageETag == nil || !validMultipartMergeObjectText(*observed.StorageETag) ||
			(observed.StorageVersionID != nil && !validMultipartMergeObjectText(*observed.StorageVersionID)))) {
		return domain.ErrInvalidStoredObjectData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, claimed.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		lockedStore := *txStore
		lockedStore.database = txStore.database.WithContext(txContext).Clauses(clause.Locking{Strength: "UPDATE"})
		current, err := lockedStore.FindUserUploadTask(txContext, claimed.UserID, claimed.ID)
		if err != nil {
			return err
		}
		if current.StagedObjectID != claimed.StagedObjectID {
			return domain.ErrUploadTaskBusy
		}
		staged, err := lockedStore.FindUserStoredObject(txContext, current.UserID, current.StagedObjectID)
		if err != nil {
			return err
		}
		matchesLocation := staged.Provider == location.Provider && staged.Bucket == location.Bucket && staged.ObjectKey == location.ObjectKey
		if current.Status == domain.UploadStatusCancelled && replayCancellation != nil {
			if !matchesLocation {
				return domain.ErrInvalidStoredObjectData
			}
			return replayCancellation(current, staged)
		}

		// 2. 拿齐行锁后读取数据库时间，复核租约、原会话绑定和对象位置。
		var databaseTime time.Time
		clockError := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&databaseTime)
		if err := txContext.Err(); err != nil {
			return err
		}
		if clockError != nil {
			return fmt.Errorf("读取分片收尾观察复核时间失败: %w", clockError)
		}
		if err := validateMultipartUploadCleanupLease(current, claimed, databaseTime, pendingStatus); err != nil {
			return err
		}
		if !validMultipartUploadCleanupObject(current, staged) || !matchesLocation {
			return domain.ErrInvalidStoredObjectData
		}

		// 3. 核对对象观察与已记录的版本、ETag 和大小，相符时进入事务回调。
		if !matchesMultipartUploadCleanupObservation(staged, observed) {
			return domain.ErrInvalidStoredObjectData
		}
		return action(txContext, txStore, current, staged)
	})
}

func matchesMultipartUploadCleanupObservation(staged *domain.StoredObject, observed *domain.UploadObjectResult) bool {
	// 未记录存储结果时，观察提供对象的实际大小或明确缺失。
	if staged.Status == domain.StoredObjectStatusStaging && staged.StorageETag == nil && staged.StorageVersionID == nil {
		return true
	}
	// 已记录的存储事实需要同一版本、ETag 和大小的对象。保留 nil 与显式 null 版本的差别。
	return observed != nil && staged.StorageETag != nil && staged.FileSize == observed.SizeBytes &&
		sameUploadStorageText(staged.StorageETag, observed.StorageETag) &&
		sameUploadStorageText(staged.StorageVersionID, observed.StorageVersionID)
}
