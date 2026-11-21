package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadPartSigningTarget 保存分片签发、回执核对及后台合并共用的计划和存储位置。
type UploadPartSigningTarget struct {
	TaskID         uint64
	UserID         uint64
	StagedObjectID uint64
	LeaseVersion   uint64
	ObjectKey      string
	UploadID       string
	FileSizeBytes  uint64
	PartSizeBytes  uint64
	PartCount      uint32
	ExpiresAt      time.Time
}

// UploadPartSigningTargetService 读取分片签发及回执核对所用的任务和暂存对象。
type UploadPartSigningTargetService struct {
	provider, bucket string
	findTask         func(context.Context, uint64, uint64) (*domain.UploadTask, time.Time, error)
	findObject       func(context.Context, uint64, uint64) (*domain.StoredObject, error)
}

// NewUploadPartSigningTargetService 装配任务和对象的归属读取及存储客户端配置。
func NewUploadPartSigningTargetService(
	provider, bucket string,
	findTask func(context.Context, uint64, uint64) (*domain.UploadTask, time.Time, error),
	findObject func(context.Context, uint64, uint64) (*domain.StoredObject, error),
) *UploadPartSigningTargetService {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if findTask == nil || findObject == nil || !location.IsValid() {
		panic("分片签发目标服务需要有效存储位置、任务查询和对象查询")
	}
	return &UploadPartSigningTargetService{provider: provider, bucket: bucket, findTask: findTask, findObject: findObject}
}

// Get 按可信 Actor 的账号获取分片签发目标。调用方交付签名之前仍需复查任务状态。
func (targetService *UploadPartSigningTargetService) Get(ctx context.Context, actor domain.Actor, taskID uint64) (*UploadPartSigningTarget, error) {
	return targetService.readTarget(ctx, actor, taskID, ValidateUploadPartSigningState)
}

// GetForReceipts 按可信 Actor 的账号获取只读回执核对目标。
// 调用方在分页前和交付前读取目标，用同一组字段复查当前任务及存储位置。
func (targetService *UploadPartSigningTargetService) GetForReceipts(ctx context.Context, actor domain.Actor, taskID uint64) (*UploadPartSigningTarget, error) {
	// 1. 采用只读资格检查，暂存对象不可用时统一判定任务尚不可核对。
	target, err := targetService.readTarget(ctx, actor, taskID, ValidateUploadPartReceiptState)
	if errors.Is(err, domain.ErrStoredObjectNotFound) || errors.Is(err, domain.ErrInvalidStoredObjectData) {
		return nil, domain.ErrUploadTaskNotReady
	}
	return target, err
}

// GetForCompletion 根据已提交的核对租约读取本次完成请求的存储目标。
// 租约以任务查询时的数据库时钟判断。完成快照写入仍须在事务内复查。
func (targetService *UploadPartSigningTargetService) GetForCompletion(
	ctx context.Context, actor domain.Actor, claimed *domain.UploadTask,
) (*UploadPartSigningTarget, error) {
	// 1. 核对租约快照的归属，按账号重读任务。
	if claimed == nil || claimed.ID == 0 || claimed.UserID == 0 || claimed.LeaseOwner == nil || claimed.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if claimed.UserID != actor.UserID {
		return nil, domain.ErrUploadTaskNotFound
	}
	target, err := targetService.readTarget(ctx, actor, claimed.ID,
		func(current *domain.UploadTask, userID, taskID uint64, databaseTime time.Time) error {
			// 2. 用数据库时间复核租约、会话和分片计划。
			return validateUploadPartTargetLease(current, claimed, userID, taskID, databaseTime, domain.UploadStatusUploading)
		})

	// 3. 复用暂存对象检查，返回已确认的存储目标。
	if errors.Is(err, domain.ErrStoredObjectNotFound) || errors.Is(err, domain.ErrInvalidStoredObjectData) {
		return nil, domain.ErrUploadTaskNotReady
	}
	return target, err
}

// GetForMerge 根据内部领取的 COMPLETING 租约读取暂存对象的存储目标。
// 目标已核对到配置的存储提供方及桶。后续存储检查和结果写回仍须遵守当前租约。
func (targetService *UploadPartSigningTargetService) GetForMerge(ctx context.Context, claimed *domain.UploadTask) (*UploadPartSigningTarget, error) {
	// 1. 用数据库时间复核任务、合并租约和绑定会话。
	if claimed == nil || claimed.ID == 0 || claimed.UserID == 0 || claimed.LeaseOwner == nil || claimed.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	validateLease := func(current *domain.UploadTask, userID, taskID uint64, databaseTime time.Time) error {
		return validateUploadPartTargetLease(current, claimed, userID, taskID, databaseTime, domain.UploadStatusCompleting)
	}
	// 2. 按任务归属读取暂存对象，核对配置桶、对象键和状态。
	target, err := targetService.readTargetForUser(ctx, claimed.UserID, claimed.ID, validateLease)
	if err != nil {
		return nil, err
	}
	// 3. 读取对象后再次复核租约，通过后返回存储目标。
	if _, err := targetService.readTargetTask(ctx, target.UserID, target.TaskID, validateLease); err != nil {
		return nil, err
	}
	return target, nil
}

// validateUploadPartTargetLease 复查任务阶段、会话及租约，再核对原分片声明。
// 调用者已确认 claimed 包含租约持有者和截止时间。
func validateUploadPartTargetLease(current, claimed *domain.UploadTask, userID, taskID uint64, databaseTime time.Time, status domain.UploadStatus) error {
	if err := validateUploadPartTargetState(current, userID, taskID, databaseTime, status); err != nil {
		return err
	}
	if err := validateUploadPartTargetState(claimed, userID, taskID, databaseTime, status); err != nil {
		return err
	}
	if current.LeaseOwner == nil || *current.LeaseOwner != *claimed.LeaseOwner || current.LeaseVersion != claimed.LeaseVersion ||
		!current.LeaseUntil.Equal(*claimed.LeaseUntil) || !databaseTime.Before(*current.LeaseUntil) ||
		current.StagedObjectID != claimed.StagedObjectID || *current.StorageUploadID != *claimed.StorageUploadID {
		return domain.ErrUploadTaskBusy
	}
	if current.FileSize != claimed.FileSize || *current.PartSize != *claimed.PartSize ||
		*current.PartCount != *claimed.PartCount || *current.ExpectedSHA256 != *claimed.ExpectedSHA256 ||
		!current.ExpiresAt.Equal(claimed.ExpiresAt) {
		return domain.ErrUploadTaskNotReady
	}
	return nil
}

func (targetService *UploadPartSigningTargetService) readTarget(
	ctx context.Context, actor domain.Actor, taskID uint64,
	validateState func(*domain.UploadTask, uint64, uint64, time.Time) error,
) (*UploadPartSigningTarget, error) {
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || taskID == 0 {
		return nil, fmt.Errorf("读取分片目标需要上下文、完整认证身份和非零任务 ID")
	}
	return targetService.readTargetForUser(ctx, actor.UserID, taskID, validateState)
}

func (targetService *UploadPartSigningTargetService) readTargetForUser(
	ctx context.Context, userID, taskID uint64,
	validateState func(*domain.UploadTask, uint64, uint64, time.Time) error,
) (*UploadPartSigningTarget, error) {
	// 1. 按可信账号读取当前任务和数据库时间，检查当前操作的资格。
	task, err := targetService.readTargetTask(ctx, userID, taskID, validateState)
	if err != nil {
		return nil, err
	}

	// 2. 按同一账号读取暂存对象，核对状态、长度和存储位置。
	object, queryError := targetService.findObject(ctx, userID, task.StagedObjectID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取分片任务的暂存对象失败: %w", queryError)
	}
	if object == nil || object.ID != task.StagedObjectID || object.OwnerUserID != userID ||
		object.Status != domain.StoredObjectStatusStaging || object.FileSize != task.FileSize ||
		object.Provider != targetService.provider || object.Bucket != targetService.bucket ||
		object.StorageVersionID != nil || object.StorageETag != nil || object.VerifiedSHA256 != nil ||
		object.VerifiedAt != nil || object.DedupHash != nil || object.UnreferencedAt != nil || object.GCAfter != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}
	location := domain.CreateStagedObjectInput{Provider: object.Provider, Bucket: object.Bucket, ObjectKey: object.ObjectKey}
	if !location.IsValid() || strings.TrimSpace(location.ObjectKey) == "" || strings.ContainsFunc(location.ObjectKey, unicode.IsControl) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 用已确认的值构造独立目标，保留期限和租约版本供调用方复查。
	return &UploadPartSigningTarget{
		TaskID: task.ID, UserID: task.UserID, StagedObjectID: object.ID, LeaseVersion: task.LeaseVersion,
		ObjectKey: object.ObjectKey, UploadID: *task.StorageUploadID,
		FileSizeBytes: task.FileSize, PartSizeBytes: *task.PartSize, PartCount: *task.PartCount, ExpiresAt: task.ExpiresAt,
	}, nil
}

func (targetService *UploadPartSigningTargetService) readTargetTask(
	ctx context.Context, userID, taskID uint64,
	validateState func(*domain.UploadTask, uint64, uint64, time.Time) error,
) (*domain.UploadTask, error) {
	if ctx == nil {
		return nil, fmt.Errorf("读取分片目标需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	task, databaseTime, queryError := targetService.findTask(ctx, userID, taskID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取分片任务失败: %w", queryError)
	}
	if err := validateState(task, userID, taskID, databaseTime); err != nil {
		return nil, err
	}
	return task, nil
}
