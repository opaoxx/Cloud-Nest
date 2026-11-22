package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ClearPublishedObjectGCTimer 清除已有活动文件引用的发布对象回收计时。
// 调用方在同一用户事务中创建文件或更新内容引用，结果由外层事务确认。
func (store *Store) ClearPublishedObjectGCTimer(ctx context.Context, objectID, entryID uint64) (*domain.StoredObject, error) {
	// 1. 核对用户事务和文件、对象标识。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil || objectID == 0 || entryID == 0 {
		return nil, fmt.Errorf("清除发布对象回收计时需要用户事务、上下文和非零对象及文件 ID")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("清除发布对象回收计时必须使用事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	userID := store.loginUserID

	// 2. 锁定活动文件引用，复查 READY 对象。
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	var reference models.FileEntry
	queryError := lockedStore.database.WithContext(ctx).Select("id", "file_size").
		Where("user_id = ? AND id = ? AND object_id = ? AND entry_type = 'FILE' AND status = 'ACTIVE'", userID, entryID, objectID).
		Take(&reference).Error
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrFileEntryNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取发布对象的文件引用失败: %w", queryError)
	}
	if reference.ID != entryID {
		return nil, domain.ErrInvalidFileEntryData
	}
	object, err := lockedStore.FindUserStoredObject(ctx, userID, objectID)
	if err != nil {
		return nil, err
	}
	if object.Status != domain.StoredObjectStatusReady || object.FileSize != reference.FileSize {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 清空两项回收计时并读回结果。
	if object.UnreferencedAt == nil {
		return object, nil
	}
	result := store.database.WithContext(ctx).Exec(
		"UPDATE stored_objects SET unreferenced_at = NULL, gc_after = NULL, updated_at = UTC_TIMESTAMP(6) "+
			"WHERE owner_user_id = ? AND id = ? AND status = 'READY' AND unreferenced_at = ? AND gc_after = ?",
		userID, objectID, *object.UnreferencedAt, *object.GCAfter)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("清除发布对象回收计时失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidStoredObjectData
	}
	cleared, err := lockedStore.FindUserStoredObject(ctx, userID, objectID)
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err != nil {
		return nil, err
	}
	if cleared.Status != domain.StoredObjectStatusReady || cleared.FileSize != reference.FileSize ||
		cleared.UnreferencedAt != nil || cleared.GCAfter != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}
	return cleared, nil
}
