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

// StartReadyObjectGCTimer 在用户事务内为已释放有效引用的 READY 对象开始计时。
// 调用方将错误交回外层事务，已有合法计时重复调用时保留原值。
func (store *Store) StartReadyObjectGCTimer(ctx context.Context, objectID uint64) (*domain.StoredObject, error) {
	// 1. 校验事务和对象 ID，按所属用户锁定 READY 对象。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil || objectID == 0 {
		return nil, fmt.Errorf("启动对象回收计时需要用户事务、上下文和非零对象 ID")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("启动对象回收计时必须使用事务 Store")
	}
	userID := store.loginUserID
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	object, err := lockedStore.FindUserStoredObject(ctx, userID, objectID)
	if err != nil {
		return nil, err
	}
	if object.Status != domain.StoredObjectStatusReady {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 2. 复核文件、上传任务及历史版本的有效引用。
	if err := store.checkStoredObjectUnreferenced(ctx, object); err != nil {
		return nil, err
	}

	// 3. 沿用已有计时，或按数据库时间写入新的七天期限。
	const gracePeriod = 7 * 24 * time.Hour
	if object.UnreferencedAt != nil {
		if object.GCAfter.Before(object.UnreferencedAt.Add(gracePeriod)) {
			return nil, domain.ErrInvalidStoredObjectData
		}
		return object, nil
	}
	result := store.database.WithContext(ctx).Exec(`
		UPDATE stored_objects
		SET unreferenced_at = UTC_TIMESTAMP(6), gc_after = DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 7 DAY),
		    updated_at = UTC_TIMESTAMP(6)
		WHERE owner_user_id = ? AND id = ? AND status = 'READY'
		  AND unreferenced_at IS NULL AND gc_after IS NULL`, userID, objectID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("启动 READY 对象回收计时失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 4. 读回计时结果，交由外层事务确认提交。
	timed, err := lockedStore.FindUserStoredObject(ctx, userID, objectID)
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err != nil {
		return nil, err
	}
	if timed.Status != domain.StoredObjectStatusReady || timed.UnreferencedAt == nil || timed.GCAfter == nil ||
		!timed.UnreferencedAt.Equal(timed.UpdatedAt) || !timed.GCAfter.Equal(timed.UnreferencedAt.Add(gracePeriod)) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	return timed, nil
}
