package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadCancellationStorageService 串联单条取消任务的存储中止与复查。
type MultipartUploadCancellationStorageService struct {
	findTarget func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error)
	abort      func(context.Context, string, string) error
	check      func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error)
	statObject func(context.Context, string) (*domain.UploadObjectResult, error)
}

// NewMultipartUploadCancellationStorageService 绑定使用同一存储配置的目标读取和存储操作。
func NewMultipartUploadCancellationStorageService(
	findTarget func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error),
	abort func(context.Context, string, string) error,
	check func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error),
	statObject func(context.Context, string) (*domain.UploadObjectResult, error),
) *MultipartUploadCancellationStorageService {
	if findTarget == nil || abort == nil || check == nil || statObject == nil {
		panic("分片取消的存储收尾需要目标读取、会话中止、复查和对象查询方法")
	}
	return &MultipartUploadCancellationStorageService{findTarget: findTarget, abort: abort, check: check, statObject: statObject}
}

// ObserveBoundSession 在事务外处理已领取租约的原会话。nil, nil 表示仍待确认。
// 返回的观察交给后续事务复核。尚未绑定会话的任务返回 ErrUploadTaskNotReady。
func (execution *MultipartUploadCancellationStorageService) ObserveBoundSession(
	ctx context.Context, claimed *domain.UploadTask,
) (*domain.MultipartUploadCancellationObservation, error) {
	// 1. 用当前租约读取并固定原对象键和会话 ID。
	if err := validateCancellationStorageClaim(ctx, claimed); err != nil {
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
	objectKey, err := execution.readCancellationObjectKey(ctx, task)
	if err != nil {
		return nil, err
	}

	// 2. 仅中止一次。存储确认或明确报告会话缺失后，复查原会话与原对象。
	abortError := execution.abort(ctx, objectKey, uploadID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if abortError != nil && !errors.Is(abortError, domain.ErrUploadPartSessionMissing) {
		return nil, multipartCancellationStorageFailure(abortError)
	}
	checked, err := execution.check(ctx, objectKey, uploadID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, multipartCancellationStorageFailure(err)
	}

	// 3. 再读任务和原绑定，只有相符且存储已确认时才交付本轮观察。
	if err := execution.recheckCancellationObjectKey(ctx, task, objectKey); err != nil {
		return nil, err
	}
	if checked == nil {
		return nil, nil
	}
	return newMultipartCancellationObservation(task, objectKey, checked.Object), nil
}

// ObserveUnboundSession 只读复查尚未绑定会话的取消任务，返回对象缺失或已有元数据的观察。
// 查询范围为暂存对象键，未关联的分片会话仍由独立流程处理。
func (execution *MultipartUploadCancellationStorageService) ObserveUnboundSession(
	ctx context.Context, claimed *domain.UploadTask,
) (*domain.MultipartUploadCancellationObservation, error) {
	// 1. 按当前取消租约读取原对象键，确认会话尚未绑定。
	if err := validateCancellationStorageClaim(ctx, claimed); err != nil {
		return nil, err
	}
	if claimed.StorageUploadID != nil {
		return nil, domain.ErrUploadTaskNotReady
	}
	task := copyUploadTask(claimed)
	objectKey, err := execution.readCancellationObjectKey(ctx, task)
	if err != nil {
		return nil, err
	}

	// 2. 只读查询原对象，区分对象存在、明确缺失和查询失败。
	object, statError := execution.statObject(ctx, objectKey)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if statError != nil && !errors.Is(statError, domain.ErrUploadObjectMissing) {
		return nil, multipartCancellationStorageFailure(statError)
	}
	// 成功需要对象元数据，明确缺失时对象元数据应为空。
	if (statError == nil) != (object != nil) {
		return nil, domain.ErrUploadStorageUnavailable
	}

	// 3. 再次核对租约、对象键和空绑定，返回观察。
	if err := execution.recheckCancellationObjectKey(ctx, task, objectKey); err != nil {
		return nil, err
	}
	return newMultipartCancellationObservation(task, objectKey, object), nil
}

func newMultipartCancellationObservation(
	task *domain.UploadTask, objectKey string, object *domain.UploadObjectResult,
) *domain.MultipartUploadCancellationObservation {
	observation := &domain.MultipartUploadCancellationObservation{Task: *task, ObjectKey: objectKey}
	if object != nil {
		observation.Object = &domain.UploadObjectResult{SizeBytes: object.SizeBytes,
			StorageETag: copyOptionalValue(object.StorageETag), StorageVersionID: copyOptionalValue(object.StorageVersionID)}
	}
	return observation
}

func validateCancellationStorageClaim(ctx context.Context, claimed *domain.UploadTask) error {
	if ctx == nil || claimed == nil || claimed.ID == 0 || claimed.UserID == 0 || claimed.StagedObjectID == 0 ||
		claimed.Status != domain.UploadStatusCancelling || claimed.UploadMode != "MULTIPART" ||
		claimed.LeaseVersion == 0 || claimed.LeaseOwner == nil || claimed.LeaseUntil == nil {
		return domain.ErrInvalidUploadTaskData
	}
	return ctx.Err()
}

func (execution *MultipartUploadCancellationStorageService) recheckCancellationObjectKey(
	ctx context.Context, task *domain.UploadTask, objectKey string,
) error {
	currentKey, err := execution.readCancellationObjectKey(ctx, task)
	if err != nil {
		return err
	}
	if currentKey != objectKey {
		return domain.ErrUploadTaskBusy
	}
	return nil
}

func (execution *MultipartUploadCancellationStorageService) readCancellationObjectKey(
	ctx context.Context, task *domain.UploadTask,
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
	if !sameMultipartCancellationSession(target.UploadID, task.StorageUploadID) {
		return "", domain.ErrUploadTaskBusy
	}
	return target.ObjectKey, nil
}

func sameMultipartCancellationSession(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// multipartCancellationStorageFailure 标记存储调用自身的超时，保留原始原因。
// 目标查询等数据库调用的超时由各自入口返回。
func multipartCancellationStorageFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, domain.ErrUploadStorageUnavailable) {
		return errors.Join(domain.ErrUploadStorageUnavailable, err)
	}
	return err
}
