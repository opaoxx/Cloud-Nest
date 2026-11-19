package repository

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SelectUploadPublicationObject 为后续目录项选择在当前事务内可引用的 READY 对象。
// task 和 staged 来自写回复核回调，后续发布动作继续使用同一事务。
func (store *Store) SelectUploadPublicationObject(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest) (*domain.StoredObject, error) {
	// 1. 核对事务和已校验的上传数据。
	if err := store.validateUploadPublicationInput(ctx, task, staged, digest); err != nil {
		return nil, err
	}

	// 2. 优先复用已有的 READY 对象。
	candidate, err := store.FindReusableUploadObject(ctx, digest)
	if err != nil || candidate != nil {
		return candidate, err
	}

	// 3. 未命中时推进暂存对象，遇到去重冲突时重新查找一次。
	promoted, promotionError := store.promoteVerifiedUploadObject(ctx, task, staged, digest)
	if !errors.Is(promotionError, domain.ErrStoredObjectDedupConflict) {
		return promoted, promotionError
	}
	candidate, err = store.FindReusableUploadObject(ctx, digest)
	if err != nil || candidate != nil {
		return candidate, err
	}
	return nil, promotionError
}
