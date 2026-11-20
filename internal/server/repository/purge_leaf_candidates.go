package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ListPurgeLeafCandidates 读取已锁定清理根项下本批可处理的叶子。
// 调用方使用 WithPurgeRootTransaction 传入的上下文和 Store。
func (store *Store) ListPurgeLeafCandidates(ctx context.Context, limit int) ([]domain.PurgeLeafCandidate, error) {
	// 1. 核对事务绑定的清理根项和批量大小。
	if limit < 1 || limit > domain.MaxPurgeLeafCandidates {
		return nil, fmt.Errorf("读取清理叶子需要 1～%d 条的批量大小", domain.MaxPurgeLeafCandidates)
	}
	root, err := store.requirePurgeRoot(ctx)
	if err != nil {
		return nil, err
	}

	// 2. 读取完整子树，标记仍有待清理子项的父目录。
	rows, err := store.readUserPurgeSubtreeRows(ctx, root.UserID, root.EntryID)
	if errors.Is(err, domain.ErrFileEntryNotFound) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if err != nil {
		return nil, err
	}
	pendingParents := make(map[uint64]bool)
	for _, row := range rows {
		if row.ID == root.EntryID {
			if row.Status != "PURGING" || row.ParentID == nil || *row.ParentID != root.ParentID || row.Revision != root.Revision {
				return nil, domain.ErrInvalidFileEntryData
			}
			continue
		}
		if row.Status != "PURGED" {
			pendingParents[*row.ParentID] = true
		}
	}

	// 3. 按 ID 选取可处理的项目并限量返回。
	candidates := make([]domain.PurgeLeafCandidate, 0, limit)
	for _, row := range rows {
		hasPendingChild := pendingParents[row.ID]
		if row.Status == "PURGED" && hasPendingChild {
			return nil, domain.ErrInvalidFileEntryData
		}
		if row.Status == "PURGED" || hasPendingChild || len(candidates) == limit {
			continue
		}
		candidates = append(candidates, domain.PurgeLeafCandidate{
			EntryID: row.ID, ParentID: *row.ParentID, EntryType: row.EntryType,
			Status: row.Status, Revision: row.Revision, FileSize: row.FileSize,
		})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}
