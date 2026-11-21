package service

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadPartSigningTarget 保存当前任务的分片计划和已核对的存储位置。
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

// UploadPartSigningTargetService 读取并校验分片签发所用的任务和暂存对象。
type UploadPartSigningTargetService struct {
	provider, bucket string
	findTask         func(context.Context, uint64, uint64) (*domain.UploadTask, time.Time, error)
	findObject       func(context.Context, uint64, uint64) (*domain.StoredObject, error)
}

// NewUploadPartSigningTargetService 装配任务和对象的归属读取及签名客户端的存储位置。
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
	// 1. 按认证账号读取当前任务和数据库时间，检查签发资格。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || taskID == 0 {
		return nil, fmt.Errorf("读取分片签发目标需要上下文、完整认证身份和非零任务 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	task, databaseTime, queryError := targetService.findTask(ctx, actor.UserID, taskID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取分片签发任务失败: %w", queryError)
	}
	if err := ValidateUploadPartSigningState(task, actor.UserID, taskID, databaseTime); err != nil {
		return nil, err
	}

	// 2. 按同一账号读取暂存对象，核对状态、长度和存储位置。
	object, queryError := targetService.findObject(ctx, actor.UserID, task.StagedObjectID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取分片签发的暂存对象失败: %w", queryError)
	}
	if object == nil || object.ID != task.StagedObjectID || object.OwnerUserID != actor.UserID ||
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

	// 3. 用已确认的值构造目标，保留本次租约版本供签名交付前复查。
	return &UploadPartSigningTarget{
		TaskID: task.ID, UserID: task.UserID, StagedObjectID: object.ID, LeaseVersion: task.LeaseVersion,
		ObjectKey: object.ObjectKey, UploadID: *task.StorageUploadID,
		FileSizeBytes: task.FileSize, PartSizeBytes: *task.PartSize, PartCount: *task.PartCount, ExpiresAt: task.ExpiresAt,
	}, nil
}
