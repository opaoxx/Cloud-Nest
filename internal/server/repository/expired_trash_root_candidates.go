package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListExpiredTrashRootCandidates 只读扫描超过保留期的顶层回收项。
// 结果供后续所属用户事务复核，可能随目录状态变化而失效。
func (store *Store) ListExpiredTrashRootCandidates(ctx context.Context, limit int) ([]domain.ExpiredTrashRootCandidate, error) {
	// 1. 校验事务外入口、上下文和扫描上限。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > domain.MaxExpiredTrashRootCandidates {
		return nil, fmt.Errorf("扫描到期回收站根项需要数据入口、上下文和 1～%d 条的扫描上限", domain.MaxExpiredTrashRootCandidates)
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描到期回收站根项须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 用 MySQL UTC 截止点筛出到期项，沿活动目录链查找真实根目录。
	// 只有在深度上限内到达根目录的候选入选，限量在路径过滤后执行。
	const query = `WITH RECURSIVE expired_paths AS (
		SELECT entry.id AS entry_id, entry.user_id, parent.parent_id AS next_parent_id,
		       CAST(1 AS UNSIGNED) AS depth
		FROM file_entries AS entry
		JOIN file_entries AS parent
		  ON parent.user_id = entry.user_id AND parent.id = entry.parent_id
		 AND parent.status = 'ACTIVE' AND parent.entry_type = 'DIR'
		WHERE entry.status = 'TRASHED'
		  AND entry.deleted_at < DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? DAY)
		UNION ALL
		SELECT path.entry_id, path.user_id, parent.parent_id, path.depth + 1
		FROM expired_paths AS path
		JOIN file_entries AS parent
		  ON parent.user_id = path.user_id AND parent.id = path.next_parent_id
		 AND parent.status = 'ACTIVE' AND parent.entry_type = 'DIR'
		WHERE path.depth < ?
	)
	SELECT entry.id AS entry_id, entry.user_id, entry.parent_id, entry.revision, entry.deleted_at
	FROM expired_paths AS path
	JOIN file_entries AS entry ON entry.user_id = path.user_id AND entry.id = path.entry_id
	WHERE path.next_parent_id IS NULL
	ORDER BY entry.deleted_at ASC, entry.id ASC
	LIMIT ?`
	candidates := make([]domain.ExpiredTrashRootCandidate, 0, limit)
	result := store.database.WithContext(queryContext).
		Raw(query, domain.TrashRetentionDays, domain.MaxFileEntryDepth, limit).Scan(&candidates)
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("扫描到期回收站根项失败: %w", result.Error)
	}

	// 3. 核对候选字段和顺序，供后续事务复核。
	if len(candidates) > limit {
		return nil, domain.ErrInvalidFileEntryData
	}
	for index := range candidates {
		candidate := &candidates[index]
		candidate.DeletedAt = candidate.DeletedAt.UTC()
		if !candidate.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		if index > 0 {
			previous := candidates[index-1]
			if candidate.DeletedAt.Before(previous.DeletedAt) ||
				(candidate.DeletedAt.Equal(previous.DeletedAt) && candidate.EntryID <= previous.EntryID) {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
	}
	return candidates, nil
}
