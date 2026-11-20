package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListStoredObjectGCCandidates 只读扫描已到期的对象回收候选。
// 后续处理须在所属用户事务内复核状态、计时和有效引用。
func (store *Store) ListStoredObjectGCCandidates(ctx context.Context, limit int) ([]domain.StoredObjectGCCandidate, error) {
	// 1. 校验上下文、事务外入口和数量上限。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > domain.MaxStoredObjectGCCandidates {
		return nil, fmt.Errorf("扫描对象回收候选需要数据入口、上下文和 1～%d 条的批量大小", domain.MaxStoredObjectGCCandidates)
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描对象回收候选须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 只读筛选已到期且拥有完整七天宽限期的对象。
	statuses := []domain.StoredObjectStatus{domain.StoredObjectStatusReady, domain.StoredObjectStatusOrphaned}
	candidates := make([]domain.StoredObjectGCCandidate, 0, limit)
	queryError := store.database.WithContext(queryContext).Table("stored_objects").
		Select("id AS object_id", "owner_user_id", "status", "unreferenced_at", "gc_after").
		Where("status IN ? AND gc_after <= UTC_TIMESTAMP(6)", statuses).
		Where("TIMESTAMPDIFF(DAY, unreferenced_at, gc_after) >= 7").
		Order("gc_after ASC, id ASC").Limit(limit).Find(&candidates).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描对象回收候选失败: %w", queryError)
	}

	// 3. 核对候选字段与顺序，交给后续用户事务复核。
	if len(candidates) > limit {
		return nil, domain.ErrInvalidStoredObjectData
	}
	for index := range candidates {
		candidate := &candidates[index]
		candidate.UnreferencedAt = candidate.UnreferencedAt.UTC()
		candidate.GCAfter = candidate.GCAfter.UTC()
		if !candidate.IsValid() {
			return nil, domain.ErrInvalidStoredObjectData
		}
		if index > 0 {
			previous := candidates[index-1]
			if candidate.GCAfter.Before(previous.GCAfter) ||
				(candidate.GCAfter.Equal(previous.GCAfter) && candidate.ObjectID <= previous.ObjectID) {
				return nil, domain.ErrInvalidStoredObjectData
			}
		}
	}
	return candidates, nil
}
