package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// FindUserEntrySubtreeStats 按已验证用户读取源自身及所有保留后代的数量和相对深度。
// 适配当前 Store 的连接或事务，源的可见性、账号和移动规则由调用服务检查。
func (store *Store) FindUserEntrySubtreeStats(ctx context.Context, userID, entryID uint64) (*domain.EntrySubtreeStats, error) {
	// 1. 检查用户、源 ID 和上下文，设置查询期限。
	if ctx == nil || userID == 0 || entryID == 0 {
		return nil, fmt.Errorf("查询子树统计需要上下文、非零用户 ID 和源目录项 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询子树统计已取消: %w", err)
	}

	// 2. 按用户范围递归读取子树并聚合数量与深度。
	// 多展开一层用于识别超深结构或循环，合法结果保留源自身的零深度。
	const query = `WITH RECURSIVE entry_subtree AS (
    SELECT id, user_id, entry_type,
           CAST(0 AS UNSIGNED) AS relative_depth,
           CAST(0 AS UNSIGNED) AS invalid_parent
    FROM file_entries
    WHERE user_id = ? AND id = ?
    UNION ALL
    SELECT child.id, child.user_id, child.entry_type,
           parent.relative_depth + 1,
           CASE WHEN parent.entry_type = 'DIR' THEN 0 ELSE 1 END
    FROM file_entries AS child
    INNER JOIN entry_subtree AS parent
        ON child.user_id = parent.user_id AND child.parent_id = parent.id
    WHERE child.user_id = ? AND parent.relative_depth < ?
)
SELECT COALESCE(MAX(CASE WHEN relative_depth = 0 THEN user_id END), 0) AS user_id,
       COALESCE(MAX(CASE WHEN relative_depth = 0 THEN id END), 0) AS entry_id,
       COUNT(*) AS node_count,
       COALESCE(MAX(relative_depth), 0) AS max_relative_depth,
       COALESCE(MAX(invalid_parent), 0) AS invalid_parent
FROM entry_subtree`
	var record models.EntrySubtreeStats
	result := store.database.WithContext(queryContext).
		Raw(query, userID, entryID, userID, domain.MaxFileEntryDepth+1).Scan(&record)

	// 3. 处理取消、缺失及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询子树统计已取消: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("查询子树统计失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}
	if record.NodeCount == 0 {
		return nil, domain.ErrFileEntryNotFound
	}

	// 4. 核对统计范围和结构，返回独立结果。
	if record.UserID != userID || record.EntryID != entryID || record.InvalidParent != 0 ||
		record.MaxRelativeDepth > domain.MaxFileEntryDepth || record.NodeCount <= record.MaxRelativeDepth {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &domain.EntrySubtreeStats{
		UserID: record.UserID, EntryID: record.EntryID,
		NodeCount: record.NodeCount, MaxRelativeDepth: record.MaxRelativeDepth,
	}, nil
}
