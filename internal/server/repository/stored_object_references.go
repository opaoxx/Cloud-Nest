package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// checkStoredObjectUnreferenced 检查当前仍有效的文件和任务引用。
// 调用方须在同一用户事务中先按归属锁定 object。
func (store *Store) checkStoredObjectUnreferenced(ctx context.Context, object *domain.StoredObject) error {
	// 1. 核对用户事务、上下文和对象归属。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil {
		return fmt.Errorf("检查存储对象引用需要用户事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("检查存储对象引用必须使用事务 Store")
	}
	if object == nil || object.ID == 0 || object.OwnerUserID != store.loginUserID {
		return domain.ErrInvalidStoredObjectData
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	userID, objectID := object.OwnerUserID, object.ID
	for _, scope := range []struct {
		table, condition string
		arguments        []any
	}{
		// 2. 用当前读检查尚未清理的文件引用。
		{"file_entries", "user_id = ? AND object_id = ? AND status <> 'PURGED'", []any{userID, objectID}},
		// 3. 用当前读检查未终结上传任务的暂存和发布引用。
		{"upload_tasks", "user_id = ? AND status NOT IN ('COMPLETED','CANCELLED','EXPIRED','FAILED') AND (staged_object_id = ? OR published_object_id = ?)", []any{userID, objectID, objectID}},
	} {
		var reference struct{ ID uint64 }
		queryError := store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
			Table(scope.table).Select("id").Where(scope.condition, scope.arguments...).Take(&reference).Error
		if err := ctx.Err(); err != nil {
			return err
		}
		if queryError == nil {
			return domain.ErrStoredObjectInUse
		}
		if !errors.Is(queryError, gorm.ErrRecordNotFound) {
			return fmt.Errorf("检查 %s 的存储对象有效引用失败: %w", scope.table, queryError)
		}
	}
	return nil
}
