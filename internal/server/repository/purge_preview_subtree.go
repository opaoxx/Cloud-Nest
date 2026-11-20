package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ListUserPurgePreviewSubtree 读取目标和全部保留后代，按 ID 升序返回。
// 调用方应复用检查目标时的同一只读快照或用户事务。
func (store *Store) ListUserPurgePreviewSubtree(ctx context.Context, userID, entryID uint64) ([]domain.PurgePreviewSubtreeEntry, error) {
	// 1. 校验用户、目标 ID 和查询期限。
	if ctx == nil || userID == 0 || entryID == 0 {
		return nil, fmt.Errorf("读取清理子树需要上下文、非零用户 ID 和目标 ID")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 沿同用户的父子关系递归读取，最多取 10001 条。
	// 递归部分限行数，并多读一层以发现超深结构。
	const query = `WITH RECURSIVE purge_subtree AS (
    SELECT id, user_id, parent_id, entry_type, status, revision, file_size,
           CAST(0 AS UNSIGNED) AS relative_depth
    FROM file_entries
    WHERE user_id = ? AND id = ?
    UNION ALL
    SELECT child.id, child.user_id, child.parent_id, child.entry_type, child.status,
           child.revision, child.file_size, parent.relative_depth + 1
    FROM file_entries AS child
    INNER JOIN purge_subtree AS parent
        ON child.user_id = parent.user_id AND child.parent_id = parent.id
    WHERE child.user_id = ? AND parent.relative_depth < ?
    LIMIT ?
)
SELECT id, user_id, parent_id, entry_type, status, revision, file_size, relative_depth
FROM purge_subtree
ORDER BY id`
	var rows []models.PurgePreviewSubtreeRow
	result := store.database.WithContext(queryContext).
		Raw(query, userID, entryID, userID, domain.MaxFileEntryDepth+1, domain.MaxFileEntrySubtreeNodes+1).Scan(&rows)
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("读取清理子树失败: %w", result.Error)
	}
	if len(rows) == 0 {
		return nil, domain.ErrFileEntryNotFound
	}

	// 3. 超过 10000 条就返回范围错误，否则核对子树结构并交付记录。
	if len(rows) > domain.MaxFileEntrySubtreeNodes {
		return nil, domain.ErrFileEntrySubtreeTooLarge
	}
	byID := make(map[uint64]*models.PurgePreviewSubtreeRow, len(rows))
	entries := make([]domain.PurgePreviewSubtreeEntry, 0, len(rows))
	for index := range rows {
		row := &rows[index]
		entry := domain.PurgePreviewSubtreeEntry{
			ID: row.ID, EntryType: row.EntryType, Status: row.Status, Revision: row.Revision, FileSize: row.FileSize,
		}
		if row.UserID != userID || row.RelativeDepth > domain.MaxFileEntryDepth ||
			(row.RelativeDepth == 0) != (row.ID == entryID) || !entry.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		if _, duplicate := byID[row.ID]; duplicate {
			return nil, domain.ErrInvalidFileEntryData
		}
		byID[row.ID] = row
		entries = append(entries, entry)
	}
	root := byID[entryID]
	if root == nil || (root.ParentID != nil && (*root.ParentID == 0 || byID[*root.ParentID] != nil)) {
		return nil, domain.ErrInvalidFileEntryData
	}

	for _, row := range rows {
		if row.ID != entryID {
			if row.ParentID == nil {
				return nil, domain.ErrInvalidFileEntryData
			}
			parent := byID[*row.ParentID]
			if parent == nil || parent.EntryType != "DIR" || parent.RelativeDepth+1 != row.RelativeDepth {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
	}
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
