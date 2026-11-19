package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FindReusableUploadObject 按事务用户和实测摘要查找内容相同的 READY 对象。
// 命中对象的行锁持续到外层事务结束，未命中返回 (nil, nil)。
func (store *Store) FindReusableUploadObject(ctx context.Context, digest domain.UploadContentDigest) (*domain.StoredObject, error) {
	// 1. 检查当前 Store 是否处于已锁定用户的事务。
	if store == nil || store.database == nil || ctx == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("查询可复用上传对象需要上下文和已锁定用户的事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("查询可复用上传对象必须使用事务 Store")
	}

	// 2. 按用户、去重摘要、大小和 READY 状态查询并锁定候选对象。
	query := store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("owner_user_id = ? AND dedup_hash = ? AND file_size = ? AND status = ?",
			store.loginUserID, digest.SHA256[:], digest.SizeBytes, string(domain.StoredObjectStatusReady))
	object, err := readStoredObject(ctx, query, store.loginUserID)

	// 3. 区分未命中与查询故障，复核命中对象后返回。
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if object.Status != domain.StoredObjectStatusReady || object.FileSize != digest.SizeBytes ||
		object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != digest.SHA256 {
		return nil, domain.ErrInvalidStoredObjectData
	}
	return object, nil
}
