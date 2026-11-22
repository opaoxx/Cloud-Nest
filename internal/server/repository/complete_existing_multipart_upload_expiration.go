package repository

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CompleteExistingMultipartUploadExpiration 终结原对象已经生成的分片到期任务。
// 终态、预占返还和供延迟回收使用的对象定位在同一事务提交。
func (store *Store) CompleteExistingMultipartUploadExpiration(
	ctx context.Context, provider, bucket string, observation *domain.MultipartUploadCleanupObservation,
) (*domain.UploadTask, error) {
	// 1. 确认观察包含已存在的对象。
	if observation == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if observation.Object == nil {
		return nil, domain.ErrUploadTaskNotReady
	}
	// 2. 将观察交给收尾事务，复核定位并原子提交终态、账目和回收登记。
	return store.completeMultipartUploadExpiration(ctx, provider, bucket, observation)
}
