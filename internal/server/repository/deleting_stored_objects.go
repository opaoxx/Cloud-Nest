package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// ListDeletingStoredObjects 读取已提交删除意图的对象和存储位置。
func (store *Store) ListDeletingStoredObjects(ctx context.Context, limit int) ([]domain.StoredObject, error) {
	// 1. 校验上下文、事务外入口和数量上限。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > domain.MaxStoredObjectGCCandidates {
		return nil, fmt.Errorf("扫描删除中的对象需要数据入口、上下文和 1～%d 条的批量大小", domain.MaxStoredObjectGCCandidates)
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描删除中的对象须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 只读扫描删除中的对象，按回收时间与 ID 限量读取。
	var records []models.StoredObject
	queryError := store.database.WithContext(queryContext).Select(storedObjectReadColumns[:]).
		Where("status = ?", domain.StoredObjectStatusDeleting).
		Order("gc_after ASC, id ASC").Limit(limit).Find(&records).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描删除中的对象失败: %w", queryError)
	}

	// 3. 复用单对象读取的校验，交付对象和存储位置。
	objects := make([]domain.StoredObject, 0, len(records))
	for _, record := range records {
		object, err := mapStoredObjectRecord(record)
		if err != nil {
			return nil, err
		}
		if object.Status != domain.StoredObjectStatusDeleting {
			return nil, domain.ErrInvalidStoredObjectData
		}
		objects = append(objects, *object)
	}
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	return objects, nil
}
