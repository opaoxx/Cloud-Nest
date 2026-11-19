package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MarkDeduplicatedUploadObjectOrphaned 为已完成任务留下的去重暂存副本开始回收计时。
// 调用方使用发布事务的 Store 和上下文；任务发布自己的暂存对象时返回 (nil, nil)。
func (store *Store) MarkDeduplicatedUploadObjectOrphaned(ctx context.Context, taskID uint64) (*domain.StoredObject, error) {
	// 1. 锁定任务，确认它已完成且复用了另一个对象。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil || taskID == 0 {
		return nil, fmt.Errorf("标记去重暂存副本需要用户事务、上下文和非零任务 ID")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("标记去重暂存副本必须使用事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	userID := store.loginUserID
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	task, err := lockedStore.FindUserUploadTask(ctx, userID, taskID)
	if err != nil {
		return nil, err
	}
	if task.Status != domain.UploadStatusCompleted {
		return nil, domain.ErrUploadTaskNotReady
	}
	if task.PublishedObjectID == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.StagedObjectID == *task.PublishedObjectID {
		return nil, nil
	}

	// 2. 锁定暂存副本，核对状态和大小。
	staged, err := lockedStore.FindUserStoredObject(ctx, userID, task.StagedObjectID)
	if err != nil {
		return nil, err
	}
	if staged.FileSize != task.FileSize ||
		(staged.Status != domain.StoredObjectStatusVerifying && staged.Status != domain.StoredObjectStatusOrphaned) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if staged.Status == domain.StoredObjectStatusVerifying &&
		(staged.UnreferencedAt != nil || staged.VerifiedSHA256 != nil || staged.VerifiedAt != nil) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 检查有效引用并标记 ORPHANED，交由外层事务确认。
	return lockedStore.markUnreferencedUploadObjectOrphaned(ctx, staged)
}
