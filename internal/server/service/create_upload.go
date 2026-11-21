package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateUploadTransaction 汇集上传创建所需的事务内读取和写入能力。
type CreateUploadTransaction interface {
	FileEntryPathReader
	// ReserveUserUploadSpace 为当前用户预占声明大小。
	ReserveUserUploadSpace(ctx context.Context, sizeBytes uint64) error
	// CreateUserStagedObject 创建本次尝试使用的暂存对象。
	CreateUserStagedObject(ctx context.Context, input domain.CreateStagedObjectInput) (*domain.StoredObject, error)
	// CreateUserUploadTask 引用暂存对象创建 SINGLE 上传任务。
	CreateUserUploadTask(ctx context.Context, stagedObjectID uint64, input domain.CreateUploadInput, expiresAt time.Time) (*domain.UploadTask, error)
	// CreateUserMultipartUploadTask 引用暂存对象和固定计划创建 Multipart 上传任务。
	CreateUserMultipartUploadTask(ctx context.Context, stagedObjectID uint64, input domain.CreateUploadInput, plan domain.UploadPlan, expiresAt time.Time) (*domain.UploadTask, error)
}

// CreateUploadService 组合路径校验、空间预占及暂存对象和任务创建。
type CreateUploadService struct {
	// withTransaction 同步执行用户事务，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, CreateUploadTransaction) error) error
	// provider 保存已校验的服务端存储逻辑标识。
	provider string
	// bucket 保存已校验的服务端存储桶名称。
	bucket string
	// generateObjectKey 为本次暂存尝试提供随机对象位置。
	generateObjectKey func(uint64) (string, error)
}

// NewCreateUploadService 绑定已认证事务和已校验的存储配置，装配错误触发 panic。
func NewCreateUploadService(provider, bucket string, withTransaction func(context.Context, domain.Actor, func(context.Context, CreateUploadTransaction) error) error) *CreateUploadService {
	// 1. 检查事务依赖和存储位置的列约束。
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if withTransaction == nil || !location.IsValid() {
		panic("上传创建服务需要用户事务方法和有效存储配置")
	}

	// 2. 保存配置并绑定默认对象键生成方法。
	return &CreateUploadService{
		withTransaction:   withTransaction,
		provider:          provider,
		bucket:            bucket,
		generateObjectKey: GenerateUploadObjectKey,
	}
}

// Create 在同一已认证用户事务中创建 24 小时有效的 SINGLE 任务。
// 失败返回 nil 与错误，外层提交确认后返回独立任务记录。
func (createService *CreateUploadService) Create(ctx context.Context, actor domain.Actor, input domain.CreateUploadInput) (*domain.UploadTask, error) {
	// 1. 检查认证身份、上下文和上传声明。
	if err := validateUploadCreationContext(ctx, actor); err != nil {
		return nil, err
	}
	declaration, err := ValidateCreateUploadInput(input)
	if err != nil {
		return nil, err
	}

	// 2. 在同一个用户事务中完成 SINGLE 任务创建。
	return createService.createPreparedUpload(ctx, actor, *declaration, domain.UploadPlan{Mode: "SINGLE"})
}

// CreateWithPlan 根据声明大小选择上传模式，提交后返回保存了固定计划的任务。
func (createService *CreateUploadService) CreateWithPlan(ctx context.Context, actor domain.Actor, input domain.CreateUploadInput, limits domain.UploadPlanLimits) (*domain.UploadTask, error) {
	// 1. 在进入事务前校验身份、声明和存储限制，固定上传计划。
	if err := validateUploadCreationContext(ctx, actor); err != nil {
		return nil, err
	}
	declaration, plan, err := ValidateCreateUploadInputWithPlan(input, limits)
	if err != nil {
		return nil, err
	}

	// 2. 在同一个用户事务中保存配额、暂存对象和计划对应的任务。
	return createService.createPreparedUpload(ctx, actor, *declaration, plan)
}

func validateUploadCreationContext(ctx context.Context, actor domain.Actor) error {
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return fmt.Errorf("创建上传任务需要上下文和完整认证身份")
	}
	return ctx.Err()
}

func (createService *CreateUploadService) createPreparedUpload(ctx context.Context, actor domain.Actor, declaration domain.CreateUploadInput, plan domain.UploadPlan) (*domain.UploadTask, error) {
	// 1. 进入用户事务，检查目标目录及完整祖先链。
	var prepared *domain.UploadTask
	transactionError := createService.withTransaction(ctx, actor, func(txContext context.Context, txStore CreateUploadTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("上传创建回调需要事务上下文和 Store")
		}
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, txStore)
		})
		path, err := paths.Get(txContext, actor, declaration.ParentID)
		if err != nil {
			return err
		}
		if path.Entries[len(path.Entries)-1].EntryType != "DIR" {
			return domain.ErrFileEntryNotDirectory
		}

		// 2. 检查新增文件的深度，生成对象键并预占完整声明空间。
		if len(path.Entries) > domain.MaxFileEntryDepth {
			return domain.ErrFileEntryDepthExceeded
		}
		objectKey, keyError := createService.generateObjectKey(actor.UserID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if keyError != nil {
			return fmt.Errorf("准备上传对象位置失败: %w", keyError)
		}
		objectInput := domain.CreateStagedObjectInput{
			Provider:  createService.provider,
			Bucket:    createService.bucket,
			ObjectKey: objectKey,
			SizeBytes: declaration.SizeBytes,
		}
		if !objectInput.IsValid() {
			return domain.ErrInvalidStoredObjectData
		}
		reserveError := txStore.ReserveUserUploadSpace(txContext, declaration.SizeBytes)
		if err := txContext.Err(); err != nil {
			return err
		}
		if reserveError != nil {
			return fmt.Errorf("预占上传空间失败: %w", reserveError)
		}

		// 3. 创建暂存对象，核对归属和存储位置。
		staged, createError := txStore.CreateUserStagedObject(txContext, objectInput)
		if err := txContext.Err(); err != nil {
			return err
		}
		if createError != nil {
			return fmt.Errorf("创建上传暂存对象失败: %w", createError)
		}
		if staged == nil || staged.ID == 0 || staged.OwnerUserID != actor.UserID || staged.Provider != objectInput.Provider ||
			staged.Bucket != objectInput.Bucket || staged.ObjectKey != objectKey || staged.FileSize != declaration.SizeBytes || staged.Status != domain.StoredObjectStatusStaging {
			return domain.ErrInvalidStoredObjectData
		}
		objectID := staged.ID

		// 4. 按模式创建任务，核对并复制初始结果。
		expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
		taskInput := declaration
		taskInput.ExpectedSHA256 = copyOptionalValue(declaration.ExpectedSHA256)
		status := domain.UploadStatusUploading
		var task *domain.UploadTask
		if plan.Mode == "MULTIPART" {
			status = domain.UploadStatusCreating
			task, createError = txStore.CreateUserMultipartUploadTask(txContext, objectID, taskInput, plan, expiresAt)
		} else {
			task, createError = txStore.CreateUserUploadTask(txContext, objectID, taskInput, expiresAt)
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		if createError != nil {
			return fmt.Errorf("创建 %s 上传任务失败: %w", plan.Mode, createError)
		}
		if task == nil || task.ID == 0 || task.UserID != actor.UserID || task.DeviceID != actor.DeviceID || task.StagedObjectID != objectID ||
			task.TargetParentID != declaration.ParentID || task.RequestedName != declaration.Name || task.FileSize != declaration.SizeBytes ||
			task.UploadMode != plan.Mode || task.Status != status || task.ReservedBytes != declaration.SizeBytes ||
			!task.ExpiresAt.Equal(expiresAt) || task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() ||
			(task.ExpectedSHA256 == nil) != (declaration.ExpectedSHA256 == nil) || task.StorageUploadID != nil ||
			task.PublishedObjectID != nil || task.ResolvedName != nil || task.ResultEntryID != nil || task.LeaseOwner != nil || task.LeaseUntil != nil ||
			task.LeaseVersion != 0 || task.NextAttemptAt != nil || task.AttemptCount != 0 || task.LastErrorCode != nil || task.LastErrorMessage != nil || task.CompletedAt != nil {
			return domain.ErrInvalidUploadTaskData
		}
		if task.ExpectedSHA256 != nil && *task.ExpectedSHA256 != *declaration.ExpectedSHA256 {
			return domain.ErrInvalidUploadTaskData
		}
		if plan.Mode == "MULTIPART" {
			if task.PartSize == nil || *task.PartSize != plan.PartSizeBytes || task.PartCount == nil || *task.PartCount != plan.PartCount {
				return domain.ErrInvalidUploadTaskData
			}
		} else if task.PartSize != nil || task.PartCount != nil {
			return domain.ErrInvalidUploadTaskData
		}
		prepared = copyUploadTask(task)
		return nil
	})

	// 5. 确认事务提交成功后返回任务。
	if transactionError != nil {
		return nil, fmt.Errorf("创建上传事务失败: %w", transactionError)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prepared == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return prepared, nil
}
