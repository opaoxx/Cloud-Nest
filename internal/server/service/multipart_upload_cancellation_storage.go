package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadCancellationStorageService 串联单条取消任务的存储中止与复查。
type MultipartUploadCancellationStorageService struct {
	cleanup multipartUploadCleanupStorage
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
	return &MultipartUploadCancellationStorageService{
		cleanup: multipartUploadCleanupStorage{findTarget: findTarget, abort: abort, check: check, statObject: statObject},
	}
}

// ObserveBoundSession 在事务外处理已领取租约的原会话。nil, nil 表示仍待确认。
// 返回的观察交给后续事务复核。尚未绑定会话的任务返回 ErrUploadTaskNotReady。
func (execution *MultipartUploadCancellationStorageService) ObserveBoundSession(
	ctx context.Context, claimed *domain.UploadTask,
) (*domain.MultipartUploadCancellationObservation, error) {
	return execution.cleanup.observeBoundSession(ctx, claimed, domain.UploadStatusCancelling)
}

// ObserveUnboundSession 只读复查尚未绑定会话的取消任务，返回对象缺失或已有元数据的观察。
// 查询范围为暂存对象键，未关联的分片会话仍由独立流程处理。
func (execution *MultipartUploadCancellationStorageService) ObserveUnboundSession(
	ctx context.Context, claimed *domain.UploadTask,
) (*domain.MultipartUploadCancellationObservation, error) {
	return execution.cleanup.observeUnboundSession(ctx, claimed, domain.UploadStatusCancelling)
}

func validateCancellationStorageClaim(ctx context.Context, claimed *domain.UploadTask) error {
	return validateMultipartCleanupStorageClaim(ctx, claimed, domain.UploadStatusCancelling)
}
