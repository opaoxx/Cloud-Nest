package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadCancellationStorageService 串联单条已绑定会话的存储中止与复查。
type MultipartUploadCancellationStorageService struct {
	findTarget func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error)
	abort      func(context.Context, string, string) error
	check      func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error)
}

// NewMultipartUploadCancellationStorageService 绑定使用同一存储配置的目标读取、中止和复查方法。
func NewMultipartUploadCancellationStorageService(
	findTarget func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error),
	abort func(context.Context, string, string) error,
	check func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error),
) *MultipartUploadCancellationStorageService {
	if findTarget == nil || abort == nil || check == nil {
		panic("分片取消的存储收尾需要目标读取、会话中止和复查方法")
	}
	return &MultipartUploadCancellationStorageService{findTarget: findTarget, abort: abort, check: check}
}

// ObserveBoundSession 在事务外处理已领取租约的原会话。nil, nil 表示仍待确认。
// 返回的观察交给后续事务复核。尚未绑定会话的任务返回 ErrUploadTaskNotReady。
func (execution *MultipartUploadCancellationStorageService) ObserveBoundSession(
	ctx context.Context, claimed *domain.UploadTask,
) (*domain.MultipartUploadCancellationObservation, error) {
	// 1. 用当前租约读取并固定原对象键和会话 ID。
	if ctx == nil || claimed == nil || claimed.ID == 0 || claimed.UserID == 0 || claimed.StagedObjectID == 0 ||
		claimed.Status != domain.UploadStatusCancelling || claimed.UploadMode != "MULTIPART" ||
		claimed.LeaseVersion == 0 || claimed.LeaseOwner == nil || claimed.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if claimed.StorageUploadID == nil {
		return nil, domain.ErrUploadTaskNotReady
	}
	if !domain.ValidMultipartUploadID(*claimed.StorageUploadID) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	task := copyUploadTask(claimed)
	uploadID := *task.StorageUploadID
	objectKey, err := execution.readBoundObjectKey(ctx, task, uploadID)
	if err != nil {
		return nil, err
	}

	// 2. 仅中止一次。存储确认或明确报告会话缺失后，复查原会话与原对象。
	abortError := execution.abort(ctx, objectKey, uploadID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if abortError != nil && !errors.Is(abortError, domain.ErrUploadPartSessionMissing) {
		return nil, abortError
	}
	checked, err := execution.check(ctx, objectKey, uploadID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}

	// 3. 再读任务和原绑定，只有相符且存储已确认时才交付本轮观察。
	currentKey, err := execution.readBoundObjectKey(ctx, task, uploadID)
	if err != nil {
		return nil, err
	}
	if currentKey != objectKey {
		return nil, domain.ErrUploadTaskBusy
	}
	if checked == nil {
		return nil, nil
	}
	observation := &domain.MultipartUploadCancellationObservation{Task: *task, ObjectKey: objectKey}
	if object := checked.Object; object != nil {
		observation.Object = &domain.UploadObjectResult{SizeBytes: object.SizeBytes,
			StorageETag: copyOptionalValue(object.StorageETag), StorageVersionID: copyOptionalValue(object.StorageVersionID)}
	}
	return observation, nil
}

func (execution *MultipartUploadCancellationStorageService) readBoundObjectKey(
	ctx context.Context, task *domain.UploadTask, uploadID string,
) (string, error) {
	target, err := execution.findTarget(ctx, task)
	if contextError := ctx.Err(); contextError != nil {
		return "", contextError
	}
	if err != nil {
		return "", err
	}
	if target == nil || target.ObjectKey == "" {
		return "", domain.ErrInvalidStoredObjectData
	}
	if target.UploadID == nil || *target.UploadID != uploadID {
		return "", domain.ErrUploadTaskBusy
	}
	return target.ObjectKey, nil
}
