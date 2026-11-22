package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type multipartUploadCleanupStorage struct {
	findTarget func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCleanupTarget, error)
	abort      func(context.Context, string, string) error
	check      func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error)
}

func (cleanup multipartUploadCleanupStorage) observeBoundSession(
	ctx context.Context, claimed *domain.UploadTask, pendingStatus domain.UploadStatus,
) (*domain.MultipartUploadCleanupObservation, error) {
	// 1. 按收尾租约读取原对象键和会话 ID。
	if err := validateMultipartCleanupStorageClaim(ctx, claimed, pendingStatus); err != nil {
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
	objectKey, err := cleanup.readObjectKey(ctx, task)
	if err != nil {
		return nil, err
	}

	// 2. 中止一次原会话。收到确认或会话明确缺失后，再复查会话与对象。
	abortError := cleanup.abort(ctx, objectKey, uploadID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if abortError != nil && !errors.Is(abortError, domain.ErrUploadPartSessionMissing) {
		return nil, multipartCleanupStorageFailure(abortError)
	}
	checked, err := cleanup.check(ctx, objectKey, uploadID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, multipartCleanupStorageFailure(err)
	}

	// 3. 复核租约与原绑定，仍匹配且存储已确认时交付本轮观察。
	if err := cleanup.recheckObjectKey(ctx, task, objectKey); err != nil {
		return nil, err
	}
	if checked == nil {
		return nil, nil
	}
	return newMultipartCleanupObservation(task, objectKey, checked.Object), nil
}

func newMultipartCleanupObservation(
	task *domain.UploadTask, objectKey string, object *domain.UploadObjectResult,
) *domain.MultipartUploadCleanupObservation {
	observation := &domain.MultipartUploadCleanupObservation{Task: *task, ObjectKey: objectKey}
	if object != nil {
		observation.Object = &domain.UploadObjectResult{SizeBytes: object.SizeBytes,
			StorageETag: copyOptionalValue(object.StorageETag), StorageVersionID: copyOptionalValue(object.StorageVersionID)}
	}
	return observation
}

func validateMultipartCleanupStorageClaim(ctx context.Context, claimed *domain.UploadTask, pendingStatus domain.UploadStatus) error {
	if ctx == nil || claimed == nil || claimed.ID == 0 || claimed.UserID == 0 || claimed.StagedObjectID == 0 ||
		claimed.Status != pendingStatus || claimed.UploadMode != "MULTIPART" ||
		claimed.LeaseVersion == 0 || claimed.LeaseOwner == nil || claimed.LeaseUntil == nil {
		return domain.ErrInvalidUploadTaskData
	}
	return ctx.Err()
}

func (cleanup multipartUploadCleanupStorage) recheckObjectKey(ctx context.Context, task *domain.UploadTask, objectKey string) error {
	currentKey, err := cleanup.readObjectKey(ctx, task)
	if err != nil {
		return err
	}
	if currentKey != objectKey {
		return domain.ErrUploadTaskBusy
	}
	return nil
}

func (cleanup multipartUploadCleanupStorage) readObjectKey(ctx context.Context, task *domain.UploadTask) (string, error) {
	target, err := cleanup.findTarget(ctx, task)
	if contextError := ctx.Err(); contextError != nil {
		return "", contextError
	}
	if err != nil {
		return "", err
	}
	if target == nil || target.ObjectKey == "" {
		return "", domain.ErrInvalidStoredObjectData
	}
	if !sameMultipartCleanupSession(target.UploadID, task.StorageUploadID) {
		return "", domain.ErrUploadTaskBusy
	}
	return target.ObjectKey, nil
}

func sameMultipartCleanupSession(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// multipartCleanupStorageFailure 标记存储调用自身的超时，保留原始原因。
// 目标查询等数据库调用的超时由各自入口返回。
func multipartCleanupStorageFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, domain.ErrUploadStorageUnavailable) {
		return errors.Join(domain.ErrUploadStorageUnavailable, err)
	}
	return err
}
