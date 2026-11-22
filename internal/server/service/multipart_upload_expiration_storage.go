package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadExpirationStorageService 执行分片到期任务的存储复查。
type MultipartUploadExpirationStorageService struct {
	cleanup multipartUploadCleanupStorage
}

// NewMultipartUploadExpirationStorageService 绑定使用同一存储配置的到期目标读取和存储操作。
func NewMultipartUploadExpirationStorageService(
	findTarget func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCleanupTarget, error),
	abort func(context.Context, string, string) error,
	check func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error),
	statObject func(context.Context, string) (*domain.UploadObjectResult, error),
) *MultipartUploadExpirationStorageService {
	if findTarget == nil || abort == nil || check == nil || statObject == nil {
		panic("分片到期的存储收尾需要目标读取、会话中止、复查和对象查询方法")
	}
	return &MultipartUploadExpirationStorageService{
		cleanup: multipartUploadCleanupStorage{findTarget: findTarget, abort: abort, check: check, statObject: statObject},
	}
}

// ObserveBoundSession 按已提交的到期租约复查原会话与对象。nil, nil 表示待确认。
func (execution *MultipartUploadExpirationStorageService) ObserveBoundSession(
	ctx context.Context, claimed *domain.UploadTask,
) (*domain.MultipartUploadCleanupObservation, error) {
	return execution.cleanup.observeBoundSession(ctx, claimed, domain.UploadStatusExpiring)
}

// ObserveUnboundSession 复查尚未绑定会话的到期任务，返回对象缺失或已有元数据的观察。
// 查询范围为暂存对象键，未关联的分片会话仍由独立流程处理。
func (execution *MultipartUploadExpirationStorageService) ObserveUnboundSession(
	ctx context.Context, claimed *domain.UploadTask,
) (*domain.MultipartUploadCleanupObservation, error) {
	return execution.cleanup.observeUnboundSession(ctx, claimed, domain.UploadStatusExpiring)
}
