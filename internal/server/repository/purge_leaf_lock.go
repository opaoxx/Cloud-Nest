package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// LockPurgeLeaf 锁定当前清理根项下的一条待处理叶子。
// 调用方使用 WithPurgeRootTransaction 传入的上下文和 Store，外层负责提交或回滚。
func (store *Store) LockPurgeLeaf(ctx context.Context, candidate domain.PurgeLeafCandidate) (*domain.LockedPurgeLeaf, error) {
	// 1. 核对清理事务和叶子候选。
	root, err := store.requirePurgeRoot(ctx)
	if err != nil {
		return nil, err
	}
	if !candidate.IsValid() {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 按归属锁定目录项，核对候选字段和子树范围。
	// 使用所属用户的唯一索引定位当前行，避免锁到其他用户的同一主键。
	var current domain.LockedPurgeLeaf
	readError := store.database.WithContext(ctx).
		Table("file_entries FORCE INDEX (uk_entries_owner_id)").
		Select("id AS entry_id", "user_id", "parent_id", "entry_type", "status", "revision", "file_size", "object_id").
		Where("user_id = ? AND id = ? AND parent_id IS NOT NULL", root.UserID, candidate.EntryID).
		Clauses(clause.Locking{Strength: "UPDATE"}).Take(&current).Error
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(readError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrPurgeLeafCandidateStale
	}
	if readError != nil {
		return nil, fmt.Errorf("锁定待清理叶子失败: %w", readError)
	}
	if current.UserID != root.UserID || current.EntryID != candidate.EntryID {
		return nil, domain.ErrInvalidFileEntryData
	}
	if current.PurgeLeafCandidate != candidate {
		return nil, domain.ErrPurgeLeafCandidateStale
	}
	if current.EntryID == root.EntryID {
		if current.ParentID != root.ParentID || current.Revision != root.Revision || current.Status != "PURGING" {
			return nil, domain.ErrPurgeLeafCandidateStale
		}
	} else if err := store.checkPurgeLeafAncestors(ctx, root, current.ParentID); err != nil {
		return nil, err
	}

	// 3. 复查待清理子项，返回当前对象引用和逻辑大小。
	var hasPendingChild bool
	childError := store.database.WithContext(ctx).Raw(`
		SELECT EXISTS (
			SELECT 1 FROM file_entries WHERE user_id = ? AND parent_id = ? AND status <> 'PURGED'
		)`, root.UserID, current.EntryID).Row().Scan(&hasPendingChild)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if childError != nil {
		return nil, fmt.Errorf("复查待清理子项失败: %w", childError)
	}
	if hasPendingChild {
		return nil, domain.ErrPurgeLeafCandidateStale
	}
	if !current.IsValid() {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &current, nil
}

// checkPurgeLeafAncestors 在同一用户事务内复查父链是否到达锁定的根项。
func (store *Store) checkPurgeLeafAncestors(ctx context.Context, root domain.PurgeRootCandidate, parentID uint64) error {
	// 1. 由父目录向上查询，隔离用户，并限制在既有目录深度范围内。
	var withinRoot bool
	readError := store.database.WithContext(ctx).Raw(`
		WITH RECURSIVE purge_leaf_path AS (
			SELECT id, parent_id, CAST(1 AS UNSIGNED) AS depth
			FROM file_entries
			WHERE user_id = ? AND id = ? AND entry_type = 'DIR' AND status <> 'PURGED'
			UNION ALL
			SELECT parent.id, parent.parent_id, path.depth + 1
			FROM file_entries AS parent
			JOIN purge_leaf_path AS path ON parent.id = path.parent_id
			WHERE parent.user_id = ? AND parent.entry_type = 'DIR' AND parent.status <> 'PURGED'
				AND path.id <> ? AND path.depth < ?
		)
		SELECT EXISTS (SELECT 1 FROM purge_leaf_path WHERE id = ?)`,
		root.UserID, parentID, root.UserID, root.EntryID, domain.MaxFileEntryDepth, root.EntryID).Row().Scan(&withinRoot)
	if err := ctx.Err(); err != nil {
		return err
	}
	if readError != nil {
		return fmt.Errorf("复查待清理叶子范围失败: %w", readError)
	}
	if !withinRoot {
		return domain.ErrPurgeLeafCandidateStale
	}
	return nil
}
