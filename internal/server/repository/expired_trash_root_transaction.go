package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// WithExpiredTrashRootTransaction 在所属用户事务中锁定并复核一条到期回收站候选。
// 复核通过后，回调在锁释放前执行。回调只能使用该事务的上下文和 Store。
func (store *Store) WithExpiredTrashRootTransaction(
	ctx context.Context,
	candidate domain.ExpiredTrashRootCandidate,
	action func(context.Context, *Store, domain.ExpiredTrashRootCandidate) error,
) error {
	// 1. 校验候选，进入所属用户的加锁事务。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核到期回收站根项需要数据入口、上下文和事务回调")
	}
	if !candidate.IsValid() {
		return domain.ErrInvalidFileEntryData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, candidate.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 锁定目标，用数据库当前时间核对状态和保留期，再对比扫描快照。
		targetStore := *txStore
		targetStore.database = txStore.database.WithContext(txContext).
			Table("file_entries FORCE INDEX (uk_entries_owner_id)").
			Where("status = 'TRASHED' AND deleted_at < DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? DAY)", domain.TrashRetentionDays)
		target, err := targetStore.readLockedTrashEntry(txContext, candidate.UserID, candidate.EntryID)
		if errors.Is(err, domain.ErrFileEntryNotFound) {
			return domain.ErrExpiredTrashRootCandidateStale
		}
		if err != nil {
			return err
		}
		if target.ParentID == nil || target.DeletedAt == nil {
			return domain.ErrInvalidFileEntryData
		}
		current := domain.ExpiredTrashRootCandidate{
			EntryID: target.ID, UserID: target.UserID, ParentID: *target.ParentID,
			Revision: target.Revision, DeletedAt: target.DeletedAt.UTC(),
		}
		if !current.IsValid() {
			return domain.ErrInvalidFileEntryData
		}
		if current.ParentID != candidate.ParentID || current.Revision != candidate.Revision || !current.DeletedAt.Equal(candidate.DeletedAt) {
			return domain.ErrExpiredTrashRootCandidateStale
		}

		// 3. 逐层锁读祖先，路径复核通过后才进入回调。
		if err := txStore.checkLockedActiveEntryAncestors(txContext, current.UserID, current.ParentID); errors.Is(err, domain.ErrFileEntryNotFound) {
			return domain.ErrExpiredTrashRootCandidateStale
		} else if err != nil {
			return err
		}
		return action(txContext, txStore, current)
	})
}
