package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// FindUserVisibleReusableObject 按可信用户和声明内容，只读查找有可见文件引用的 READY 对象。
// 无匹配内容或路径不可见时返回 nil, nil。创建新引用前仍须在所属用户事务中复核。
func (store *Store) FindUserVisibleReusableObject(
	ctx context.Context, userID uint64, digest [sha256.Size]byte, sizeBytes uint64,
) (*domain.StoredObject, error) {
	// 1. 校验用户范围和查询期限。
	if store == nil || store.database == nil || ctx == nil || userID == 0 {
		return nil, fmt.Errorf("查询秒传候选需要数据入口、上下文和非零用户 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 在同一条查询中匹配内容和完整活动路径。
	const query = `WITH RECURSIVE matching_object AS (
		SELECT object.id, object.owner_user_id, object.file_size
		FROM stored_objects AS object
		JOIN users AS account ON account.id = object.owner_user_id AND account.status = 'ACTIVE'
		WHERE object.owner_user_id = ? AND object.dedup_hash = ? AND object.file_size = ? AND object.status = 'READY'
	), active_paths AS (
		SELECT entry.user_id, entry.parent_id, CAST(0 AS UNSIGNED) AS depth
		FROM matching_object AS object
		JOIN file_entries AS entry
		  ON entry.user_id = object.owner_user_id AND entry.object_id = object.id AND entry.file_size = object.file_size
		WHERE entry.entry_type = 'FILE' AND entry.status = 'ACTIVE' AND entry.parent_id IS NOT NULL
		UNION DISTINCT
		SELECT parent.user_id, parent.parent_id, path.depth + 1
		FROM active_paths AS path
		JOIN file_entries AS parent
		  ON parent.user_id = path.user_id AND parent.id = path.parent_id
		 AND parent.entry_type = 'DIR' AND parent.status = 'ACTIVE'
		WHERE path.depth < ?
	)
	SELECT %s FROM stored_objects
	WHERE id = (SELECT id FROM matching_object)
	  AND EXISTS (SELECT 1 FROM active_paths WHERE parent_id IS NULL)`
	var record models.StoredObject
	result := store.database.WithContext(queryContext).
		Raw(fmt.Sprintf(query, strings.Join(storedObjectReadColumns[:], ", ")),
			userID, digest[:], sizeBytes, domain.MaxFileEntryDepth).Scan(&record)
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("查询可见秒传候选失败: %w", result.Error)
	}

	// 3. 核对对象字段，返回候选或未命中。
	if result.RowsAffected == 0 {
		return nil, nil
	}
	if result.RowsAffected != 1 || record.OwnerUserID != userID || record.Status != string(domain.StoredObjectStatusReady) ||
		record.FileSize != sizeBytes {
		return nil, domain.ErrInvalidStoredObjectData
	}
	object, err := mapStoredObjectRecord(record)
	if err != nil {
		return nil, err
	}
	if object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != digest {
		return nil, domain.ErrInvalidStoredObjectData
	}
	return object, nil
}
