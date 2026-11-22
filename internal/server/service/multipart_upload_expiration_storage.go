package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MultipartUploadExpirationStorageService 处理已绑定分片会话的到期存储复查。
type MultipartUploadExpirationStorageService struct {
	cleanup multipartUploadCleanupStorage
}

// NewMultipartUploadExpirationStorageService 绑定使用同一存储配置的到期目标读取、中止和复查。
func NewMultipartUploadExpirationStorageService(
	findTarget func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCleanupTarget, error),
	abort func(context.Context, string, string) error,
	check func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error),
) *MultipartUploadExpirationStorageService {
	if findTarget == nil || abort == nil || check == nil {
		panic("分片到期的存储收尾需要目标读取、会话中止和复查方法")
	}
	return &MultipartUploadExpirationStorageService{cleanup: multipartUploadCleanupStorage{findTarget: findTarget, abort: abort, check: check}}
}

// ObserveBoundSession 按已提交的到期租约复查原会话与对象。nil, nil 表示待确认。
func (execution *MultipartUploadExpirationStorageService) ObserveBoundSession(
	ctx context.Context, claimed *domain.UploadTask,
) (*domain.MultipartUploadCleanupObservation, error) {
	return execution.cleanup.observeBoundSession(ctx, claimed, domain.UploadStatusExpiring)
}
