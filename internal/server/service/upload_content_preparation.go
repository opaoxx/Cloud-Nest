package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadContentPreparationTransaction 汇集内容接收准备所需的事务内能力。
type UploadContentPreparationTransaction interface {
	// LockUserUploadTask 当前读锁定已认证用户的上传任务。
	LockUserUploadTask(ctx context.Context, taskID uint64) (*domain.UploadTask, error)
	// FindUserStoredObject 按可信用户与对象标识读取完整记录。
	FindUserStoredObject(ctx context.Context, userID, objectID uint64) (*domain.StoredObject, error)
	// ClaimUserUploadContentLease 使用数据库时间领取当前任务租约。
	ClaimUserUploadContentLease(ctx context.Context, taskID, objectID, expectedVersion uint64, owner string) (*domain.UploadTask, error)
	// CreateUserStagedObject 为新接收尝试创建独立暂存对象。
	CreateUserStagedObject(ctx context.Context, input domain.CreateStagedObjectInput) (*domain.StoredObject, error)
	// ReplaceUserUploadStagedObject 按当前租约替换任务的暂存对象引用。
	ReplaceUserUploadStagedObject(ctx context.Context, taskID, oldID, newID, version uint64, owner string) (*domain.UploadTask, error)
	// MarkUserStagedObjectOrphaned 为已释放引用的旧暂存对象记录孤立状态。
	MarkUserStagedObjectOrphaned(ctx context.Context, objectID uint64) (*domain.StoredObject, error)
}

// UploadContentPreparationService 组合首次接收及重新接收的任务与对象准备。
type UploadContentPreparationService struct {
	// withTransaction 同步执行已认证用户事务，仅在提交成功后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, UploadContentPreparationTransaction) error) error
	// provider 与 bucket 保存已校验的服务端存储位置配置。
	provider, bucket string
	// generateLeaseOwner 为每次接收提供独立的服务端持有者标识。
	generateLeaseOwner func() (string, error)
	// generateObjectKey 为重试生成独立不可变对象键。
	generateObjectKey func(uint64) (string, error)
}

// NewUploadContentPreparationService 绑定已认证事务及有效存储配置。
func NewUploadContentPreparationService(provider, bucket string, withTransaction func(context.Context, domain.Actor, func(context.Context, UploadContentPreparationTransaction) error) error) *UploadContentPreparationService {
	// 1. 核对依赖及位置列约束。
	if withTransaction == nil || !(domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}).IsValid() {
		panic("上传接收准备服务需要用户事务方法和有效存储配置")
	}
	// 2. 绑定现有对象键生成方法及每次独立的随机持有者。
	return &UploadContentPreparationService{
		withTransaction: withTransaction,
		provider:        provider,
		bucket:          bucket,
		generateLeaseOwner: func() (string, error) {
			identifier, err := uuid.NewRandom()
			if err != nil {
				return "", err
			}
			return identifier.String(), nil
		},
		generateObjectKey: GenerateUploadObjectKey,
	}
}

// Prepare 为已认证用户准备当前任务的内容接收，提交后返回独立结果。
// 调用方在决定接收本次文件流后使用本方法，持久恢复沿后续恢复流程推进。
func (preparer *UploadContentPreparationService) Prepare(ctx context.Context, actor domain.Actor, taskID uint64) (*domain.UploadContentPreparation, error) {
	// 1. 检查认证身份、任务 ID 和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || taskID == 0 {
		return nil, fmt.Errorf("准备上传内容接收需要上下文、完整认证身份和非零任务 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 在用户事务中锁定任务，保存本次准备前的任务事实。
	var prepared *domain.UploadContentPreparation
	transactionError := preparer.withTransaction(ctx, actor, func(txContext context.Context, txStore UploadContentPreparationTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("上传接收准备回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		task, queryError := txStore.LockUserUploadTask(txContext, taskID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return queryError
		}
		if task == nil || task.ID != taskID || task.UserID != actor.UserID || task.DeviceID == 0 || task.StagedObjectID == 0 {
			return domain.ErrInvalidUploadTaskData
		}
		original := copyUploadTask(task)

		// 3. 生成本次持有者标识并领取接收租约。
		owner, generateError := preparer.generateLeaseOwner()
		if err := txContext.Err(); err != nil {
			return err
		}
		if generateError != nil {
			return fmt.Errorf("生成上传接收租约持有者失败: %w", generateError)
		}
		claimed, claimError := txStore.ClaimUserUploadContentLease(txContext, taskID, original.StagedObjectID, original.LeaseVersion, owner)
		if err := txContext.Err(); err != nil {
			return err
		}
		if claimError != nil {
			return claimError
		}
		if claimed == nil || claimed.UpdatedAt.IsZero() || original.LeaseVersion == ^uint64(0) || owner == "" ||
			claimed.StagedObjectID != original.StagedObjectID || claimed.DeviceID != original.DeviceID || claimed.TargetParentID != original.TargetParentID ||
			claimed.RequestedName != original.RequestedName || claimed.FileSize != original.FileSize || !claimed.ExpiresAt.Equal(original.ExpiresAt) ||
			claimed.LeaseOwner == nil || *claimed.LeaseOwner != owner || claimed.LeaseVersion != original.LeaseVersion+1 || claimed.LeaseUntil == nil ||
			!claimed.LeaseUntil.Equal(claimed.UpdatedAt.Add(time.Minute)) ||
			(claimed.ExpectedSHA256 == nil) != (original.ExpectedSHA256 == nil) {
			return domain.ErrInvalidUploadTaskData
		}
		if claimed.ExpectedSHA256 != nil && *claimed.ExpectedSHA256 != *original.ExpectedSHA256 {
			return domain.ErrInvalidUploadTaskData
		}
		if err := ValidateUploadContentState(claimed, actor.UserID, taskID, claimed.UpdatedAt); err != nil {
			return err
		}
		current := copyUploadTask(claimed)

		// 4. 核对当前暂存对象，首次接收复用初始对象。
		object, queryError := txStore.FindUserStoredObject(txContext, actor.UserID, original.StagedObjectID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return queryError
		}
		if object == nil || object.ID != original.StagedObjectID || object.OwnerUserID != actor.UserID || object.Status != domain.StoredObjectStatusStaging ||
			object.FileSize != current.FileSize || object.UnreferencedAt != nil || object.GCAfter != nil {
			return domain.ErrInvalidStoredObjectData
		}
		if original.LeaseVersion == 0 && !isInitialStagedObject(object) {
			return domain.ErrInvalidStoredObjectData
		}
		originalObject := *object

		// 5. 重新接收时创建新对象、替换引用并孤立旧对象。
		if original.LeaseVersion > 0 {
			objectKey, generateError := preparer.generateObjectKey(actor.UserID)
			if err := txContext.Err(); err != nil {
				return err
			}
			if generateError != nil {
				return fmt.Errorf("生成重新接收对象键失败: %w", generateError)
			}
			input := domain.CreateStagedObjectInput{Provider: preparer.provider, Bucket: preparer.bucket, ObjectKey: objectKey, SizeBytes: current.FileSize}
			if !input.IsValid() || (object.Provider == input.Provider && object.Bucket == input.Bucket && object.ObjectKey == input.ObjectKey) {
				return domain.ErrInvalidStoredObjectData
			}
			created, createError := txStore.CreateUserStagedObject(txContext, input)
			if err := txContext.Err(); err != nil {
				return err
			}
			if createError != nil {
				return createError
			}
			if created == nil || created.ID == 0 || created.ID == original.StagedObjectID || created.OwnerUserID != actor.UserID ||
				created.Status != domain.StoredObjectStatusStaging || created.Provider != input.Provider || created.Bucket != input.Bucket ||
				created.ObjectKey != input.ObjectKey || created.FileSize != input.SizeBytes || !isInitialStagedObject(created) {
				return domain.ErrInvalidStoredObjectData
			}
			newObjectID := created.ID
			newObject := *created
			replaced, replaceError := txStore.ReplaceUserUploadStagedObject(txContext, taskID, original.StagedObjectID, newObjectID, current.LeaseVersion, owner)
			if err := txContext.Err(); err != nil {
				return err
			}
			if replaceError != nil {
				return replaceError
			}
			if replaced == nil || replaced.StagedObjectID != newObjectID || replaced.DeviceID != current.DeviceID || replaced.TargetParentID != current.TargetParentID ||
				replaced.RequestedName != current.RequestedName || replaced.FileSize != current.FileSize || !replaced.ExpiresAt.Equal(current.ExpiresAt) ||
				replaced.LeaseOwner == nil || *replaced.LeaseOwner != owner || replaced.LeaseVersion != current.LeaseVersion || replaced.LeaseUntil == nil ||
				!replaced.LeaseUntil.Equal(*current.LeaseUntil) || replaced.UpdatedAt.Before(current.UpdatedAt) || !replaced.UpdatedAt.Before(*replaced.LeaseUntil) ||
				(replaced.ExpectedSHA256 == nil) != (current.ExpectedSHA256 == nil) {
				return domain.ErrInvalidUploadTaskData
			}
			if replaced.ExpectedSHA256 != nil && *replaced.ExpectedSHA256 != *current.ExpectedSHA256 {
				return domain.ErrInvalidUploadTaskData
			}
			if err := ValidateUploadContentState(replaced, actor.UserID, taskID, replaced.UpdatedAt); err != nil {
				return err
			}
			current = copyUploadTask(replaced)
			orphaned, orphanError := txStore.MarkUserStagedObjectOrphaned(txContext, original.StagedObjectID)
			if err := txContext.Err(); err != nil {
				return err
			}
			if orphanError != nil {
				return orphanError
			}
			if orphaned == nil || orphaned.ID != original.StagedObjectID || orphaned.OwnerUserID != actor.UserID || orphaned.Status != domain.StoredObjectStatusOrphaned ||
				orphaned.Provider != originalObject.Provider || orphaned.Bucket != originalObject.Bucket || orphaned.ObjectKey != originalObject.ObjectKey || orphaned.FileSize != originalObject.FileSize ||
				orphaned.UnreferencedAt == nil || orphaned.GCAfter == nil || orphaned.GCAfter.Before(orphaned.UnreferencedAt.Add(7*24*time.Hour)) {
				return domain.ErrInvalidStoredObjectData
			}
			object = &newObject
		}

		// 6. 核对并复制准备结果，确认提交后返回。
		if object.ID != current.StagedObjectID || object.OwnerUserID != actor.UserID || object.Status != domain.StoredObjectStatusStaging ||
			object.FileSize != current.FileSize || !isInitialStagedObject(object) {
			return domain.ErrInvalidStoredObjectData
		}
		resultObject := *object
		resultObject.StorageVersionID = copyOptionalValue(object.StorageVersionID)
		resultObject.VerifiedSHA256 = copyOptionalValue(object.VerifiedSHA256)
		resultObject.DedupHash = copyOptionalValue(object.DedupHash)
		resultObject.StorageETag = copyOptionalValue(object.StorageETag)
		resultObject.VerifiedAt = copyOptionalValue(object.VerifiedAt)
		resultObject.UnreferencedAt = copyOptionalValue(object.UnreferencedAt)
		resultObject.GCAfter = copyOptionalValue(object.GCAfter)
		resultObject.LastErrorCode = copyOptionalValue(object.LastErrorCode)
		if err := txContext.Err(); err != nil {
			return err
		}
		prepared = &domain.UploadContentPreparation{Task: copyUploadTask(current), Object: &resultObject}
		return nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if transactionError != nil {
		return nil, fmt.Errorf("准备上传内容接收事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return prepared, nil
}

// isInitialStagedObject 检查已读取对象的八项初始可空事实。
func isInitialStagedObject(object *domain.StoredObject) bool {
	// 1. 接收新尝试前，要求验证、存储和回收元数据均处于初始空值。
	return object.StorageVersionID == nil && object.VerifiedSHA256 == nil && object.DedupHash == nil && object.StorageETag == nil &&
		object.VerifiedAt == nil && object.UnreferencedAt == nil && object.GCAfter == nil && object.LastErrorCode == nil
}
