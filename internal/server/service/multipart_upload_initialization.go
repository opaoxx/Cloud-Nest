package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadInitializationProcessor 处理一条尚未绑定存储会话的分片上传候选。
type MultipartUploadInitializationProcessor struct {
	provider, bucket string
	// claimLease 和 bindSession 分别在各自数据库事务提交后返回任务。
	claimLease  func(context.Context, domain.MultipartUploadInitializationCandidate, string) (*domain.UploadTask, error)
	findObject  func(context.Context, uint64, uint64) (*domain.StoredObject, error)
	bindSession func(context.Context, *domain.UploadTask, string) (*domain.UploadTask, error)
	// createSession 绑定到 provider 和 bucket 对应的存储客户端。
	createSession func(context.Context, string) (string, error)
	// scheduleRetry 在当前租约下保存下次尝试时间，返回已提交的任务。
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error)
}

// NewMultipartUploadInitializationProcessor 组装单条候选所需的事务、查询和存储入口。
func NewMultipartUploadInitializationProcessor(
	provider, bucket string,
	claimLease func(context.Context, domain.MultipartUploadInitializationCandidate, string) (*domain.UploadTask, error),
	findObject func(context.Context, uint64, uint64) (*domain.StoredObject, error),
	createSession func(context.Context, string) (string, error),
	bindSession func(context.Context, *domain.UploadTask, string) (*domain.UploadTask, error),
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error),
) *MultipartUploadInitializationProcessor {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || claimLease == nil || findObject == nil || createSession == nil || bindSession == nil || scheduleRetry == nil {
		panic("分片上传初始化需要存储位置、租约领取、对象查询、创建会话、绑定和重试方法")
	}
	return &MultipartUploadInitializationProcessor{
		provider: provider, bucket: bucket, claimLease: claimLease, findObject: findObject,
		createSession: createSession, bindSession: bindSession, scheduleRetry: scheduleRetry,
	}
}

// Process 初始化一条候选，返回已绑定会话或已安排重试的任务。
func (processor *MultipartUploadInitializationProcessor) Process(ctx context.Context, candidate domain.MultipartUploadInitializationCandidate, leaseOwner string) (*domain.UploadTask, error) {
	// 1. 领取租约，核对已提交的任务和暂存对象。
	if ctx == nil {
		return nil, fmt.Errorf("处理分片上传初始化需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	task, err := processor.claimLease(ctx, candidate, leaseOwner)
	if err != nil {
		return nil, err
	}
	if task == nil || task.ID != candidate.TaskID || task.UserID != candidate.UserID || task.StagedObjectID != candidate.StagedObjectID ||
		task.UploadMode != "MULTIPART" || task.Status != domain.UploadStatusCreating || task.StorageUploadID != nil ||
		task.LeaseVersion == 0 || task.LeaseVersion-1 != candidate.LeaseVersion ||
		task.LeaseOwner == nil || *task.LeaseOwner != leaseOwner || task.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	object, err := processor.findObject(ctx, task.UserID, task.StagedObjectID)
	if err != nil {
		return nil, err
	}
	if object == nil || object.ID != task.StagedObjectID || object.OwnerUserID != task.UserID ||
		object.Status != domain.StoredObjectStatusStaging || object.FileSize != task.FileSize ||
		object.Provider != processor.provider || object.Bucket != processor.bucket {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 2. 在事务外用固定对象键创建 MinIO 会话。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	uploadID, createError := processor.createSession(ctx, object.ObjectKey)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 3. 存储暂不可用或存储请求自身超时时，用当前租约按退避规则登记重试。
	if createError != nil {
		if errors.Is(createError, context.Canceled) ||
			!(errors.Is(createError, domain.ErrUploadStorageUnavailable) || errors.Is(createError, context.DeadlineExceeded)) {
			return nil, fmt.Errorf("初始化分片存储会话失败: %w", createError)
		}
		scheduled, err := processor.scheduleRetry(ctx, task, uploadRetryDelaySeconds(task.AttemptCount))
		if err != nil {
			return nil, fmt.Errorf("安排分片初始化重试失败: %w", err)
		}
		if scheduled == nil || scheduled.ID != task.ID || scheduled.UserID != task.UserID || scheduled.StagedObjectID != task.StagedObjectID ||
			scheduled.Status != domain.UploadStatusCreating || scheduled.UploadMode != task.UploadMode || scheduled.StorageUploadID != nil ||
			scheduled.LeaseVersion != task.LeaseVersion || scheduled.LeaseOwner != nil || scheduled.LeaseUntil != nil ||
			scheduled.NextAttemptAt == nil {
			return nil, domain.ErrInvalidUploadTaskData
		}
		return scheduled, nil
	}
	if !domain.ValidMultipartUploadID(uploadID) {
		return nil, domain.ErrUploadStorageUnavailable
	}

	// 4. 按当前租约绑定会话，提交后返回任务。结果不明的会话由后续核对处理。
	bound, err := processor.bindSession(ctx, task, uploadID)
	if err != nil {
		return nil, fmt.Errorf("写回分片存储会话失败: %w", err)
	}
	if bound == nil || bound.ID != task.ID || bound.UserID != task.UserID || bound.StagedObjectID != task.StagedObjectID ||
		bound.UploadMode != "MULTIPART" || bound.Status != domain.UploadStatusUploading ||
		bound.StorageUploadID == nil || *bound.StorageUploadID != uploadID || bound.LeaseVersion != task.LeaseVersion ||
		bound.LeaseOwner != nil || bound.LeaseUntil != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return bound, nil
}
