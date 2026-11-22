package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecheckVisibleReusableObject 在账号事务内锁定原秒传候选并重新确认可见引用。
// 候选失效统一返回 nil, nil。通过后对象锁随外层事务释放。
func (store *Store) RecheckVisibleReusableObject(ctx context.Context, candidate *domain.StoredObject) (*domain.StoredObject, error) {
	// 1. 核对账号事务和候选归属。
	if store == nil || store.database == nil || ctx == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("复核秒传候选需要上下文和所属账号事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("复核秒传候选必须使用事务 Store")
	}
	if candidate == nil || candidate.ID == 0 || candidate.OwnerUserID == 0 ||
		candidate.Status != domain.StoredObjectStatusReady || candidate.VerifiedSHA256 == nil {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if candidate.OwnerUserID != store.loginUserID {
		return nil, nil
	}

	// 2. 按账号和对象 ID 当前读加锁，再复核内容与存储位置。
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.FindUserStoredObject(ctx, store.loginUserID, candidate.ID)
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if current.Status != domain.StoredObjectStatusReady || current.FileSize != candidate.FileSize ||
		current.VerifiedSHA256 == nil || *current.VerifiedSHA256 != *candidate.VerifiedSHA256 ||
		!sameStoredObjectLocation(current, candidate) {
		return nil, nil
	}

	// 3. 与回收站及对象回收共用账号锁。锁内复用完整可见路径查询。
	visible, err := store.FindUserVisibleReusableObject(ctx, store.loginUserID, *current.VerifiedSHA256, current.FileSize)
	if err != nil {
		return nil, err
	}
	if visible == nil || visible.ID != current.ID || !sameStoredObjectLocation(visible, current) {
		return nil, nil
	}
	return current, nil
}
