package repository

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CompletePurgeLeaf 在根项事务内完成一个叶子的逻辑清理。
// 调用方须将错误返回给 WithPurgeRootTransaction，使配额、目录项、对象计时和偏好一起回滚。
func (store *Store) CompletePurgeLeaf(ctx context.Context, candidate domain.PurgeLeafCandidate) error {
	// 1. 锁定并复核叶子，读取当前结算数据。
	leaf, err := store.LockPurgeLeaf(ctx, candidate)
	if err != nil {
		return err
	}

	// 2. 按实际文件大小扣减已用空间。
	if err := store.releasePurgeLeafQuota(ctx, leaf); err != nil {
		return err
	}

	// 3. 标记清理完成并解除文件的对象引用。
	if err := store.markPurgeLeafCompleted(ctx, leaf); err != nil {
		return err
	}

	// 4. 复核原对象，最后一个有效引用解除时开始计时。
	if leaf.ObjectID != nil {
		if _, err := store.StartReadyObjectGCTimer(ctx, *leaf.ObjectID); err != nil && !errors.Is(err, domain.ErrStoredObjectInUse) {
			return err
		}
	}

	// 5. 清除收藏和最近访问记录。
	return store.clearPurgeLeafPreference(ctx, leaf)
}
