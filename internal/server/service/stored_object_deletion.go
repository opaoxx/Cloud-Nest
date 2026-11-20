package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// StoredObjectDeletionProcessor 串联一次存储删除与完成写回。
type StoredObjectDeletionProcessor struct {
	remove   func(context.Context, *domain.StoredObject) error
	complete func(context.Context, *domain.StoredObject) (*domain.StoredObject, error)
}

// NewStoredObjectDeletionProcessor 绑定存储删除和 MySQL 写回入口。
func NewStoredObjectDeletionProcessor(
	remove func(context.Context, *domain.StoredObject) error,
	complete func(context.Context, *domain.StoredObject) (*domain.StoredObject, error),
) *StoredObjectDeletionProcessor {
	// 1. 检查并保存执行依赖。
	if remove == nil || complete == nil {
		panic("处理存储对象删除需要存储删除和完成写回方法")
	}
	return &StoredObjectDeletionProcessor{remove: remove, complete: complete}
}

// Process 处理一条已提交的 DELETING 记录，返回已确认提交的 DELETED 对象。
func (processor *StoredObjectDeletionProcessor) Process(ctx context.Context, deleting *domain.StoredObject) (*domain.StoredObject, error) {
	// 1. 校验上下文和待删除记录。
	if ctx == nil {
		return nil, errors.New("处理存储对象删除需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if deleting == nil || deleting.ID == 0 || deleting.OwnerUserID == 0 || deleting.Status != domain.StoredObjectStatusDeleting {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 2. 在数据库事务外删除指定内容。
	if err := processor.remove(ctx, deleting); err != nil {
		return nil, fmt.Errorf("删除存储对象失败: %w", err)
	}

	// 3. 存储确认后，写回并核对 DELETED 结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	completed, err := processor.complete(ctx, deleting)
	if err != nil {
		return nil, fmt.Errorf("写回对象删除完成状态失败: %w", err)
	}
	if completed == nil || completed.ID != deleting.ID || completed.OwnerUserID != deleting.OwnerUserID ||
		completed.Status != domain.StoredObjectStatusDeleted {
		return nil, domain.ErrInvalidStoredObjectData
	}
	return completed, nil
}
