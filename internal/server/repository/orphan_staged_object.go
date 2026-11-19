package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MarkUserStagedObjectOrphaned 在已认证用户事务中标记已释放引用的旧暂存对象。
// 上层先在同一事务完成任务对象切换，错误须向外传播，结果由外层提交确认。
// 已有 ORPHANED 记录保留原无引用起点与清理期限。
func (store *Store) MarkUserStagedObjectOrphaned(ctx context.Context, objectID uint64) (*domain.StoredObject, error) {
	// 1. 检查已认证事务、上下文和对象 ID。
	if ctx == nil || objectID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("标记孤立暂存对象需要上下文、非零对象 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("标记孤立暂存对象必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeContext.Err(); err != nil {
		return nil, err
	}

	// 2. 加锁读取完整对象，核对允许的暂存状态。
	lockedStore := *store
	lockedStore.database = store.database.WithContext(writeContext).Clauses(clause.Locking{Strength: "UPDATE"})
	object, err := lockedStore.FindUserStoredObject(writeContext, actor.UserID, objectID)
	if err != nil {
		return nil, err
	}
	if object.Status != domain.StoredObjectStatusStaging && object.Status != domain.StoredObjectStatusOrphaned {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if object.Status == domain.StoredObjectStatusStaging && object.UnreferencedAt != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 复用有效引用检查和孤立计时写入。
	return lockedStore.markUnreferencedUploadObjectOrphaned(writeContext, object)
}

// markUnreferencedUploadObjectOrphaned 处理已锁定且阶段已复核的上传对象。
func (lockedStore *Store) markUnreferencedUploadObjectOrphaned(ctx context.Context, object *domain.StoredObject) (*domain.StoredObject, error) {
	userID, objectID := object.OwnerUserID, object.ID
	// 1. 当前读检查文件与未终结上传任务的有效引用。
	for _, scope := range []struct {
		table, condition string
		arguments        []any
	}{
		{"file_entries", "user_id = ? AND object_id = ? AND status <> 'PURGED'", []any{userID, objectID}},
		{"upload_tasks", "user_id = ? AND status NOT IN ('COMPLETED','CANCELLED','EXPIRED','FAILED') AND (staged_object_id = ? OR published_object_id = ?)", []any{userID, objectID, objectID}},
	} {
		var reference struct{ ID uint64 }
		queryError := lockedStore.database.WithContext(ctx).Table(scope.table).Select("id").
			Where(scope.condition, scope.arguments...).Take(&reference).Error
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if queryError == nil {
			return nil, domain.ErrStoredObjectInUse
		}
		if !errors.Is(queryError, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("检查暂存对象有效引用失败: %w", queryError)
		}
	}

	// 2. 复用已有孤立记录，或条件写入孤立状态及七天宽限期。
	if object.UnreferencedAt != nil {
		if object.GCAfter.Before(object.UnreferencedAt.Add(7 * 24 * time.Hour)) {
			return nil, domain.ErrInvalidStoredObjectData
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return object, nil
	}
	result := lockedStore.database.WithContext(ctx).Exec(
		"UPDATE stored_objects SET status = 'ORPHANED', unreferenced_at = UTC_TIMESTAMP(6), "+
			"gc_after = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 7 DAY), updated_at = UTC_TIMESTAMP(6) "+
			"WHERE owner_user_id = ? AND id = ? AND status = ? AND unreferenced_at IS NULL AND gc_after IS NULL",
		userID, objectID, string(object.Status))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("标记孤立暂存对象失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 当前读复核写入结果，交由外层事务确认。
	orphaned, err := lockedStore.FindUserStoredObject(ctx, userID, objectID)
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err != nil {
		return nil, err
	}
	if orphaned.Status != domain.StoredObjectStatusOrphaned || orphaned.UnreferencedAt == nil || orphaned.GCAfter == nil ||
		!orphaned.UnreferencedAt.Equal(orphaned.UpdatedAt) || !orphaned.GCAfter.Equal(orphaned.UnreferencedAt.Add(7*24*time.Hour)) ||
		orphaned.Provider != object.Provider || orphaned.Bucket != object.Bucket || orphaned.ObjectKey != object.ObjectKey || orphaned.FileSize != object.FileSize {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return orphaned, nil
}
