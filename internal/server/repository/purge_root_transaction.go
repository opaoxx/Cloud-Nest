package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// WithPurgeRootTransaction 锁定并复核待清理根项，在同一用户事务内执行回调。
// 候选来自内部扫描。回调只使用传入的事务上下文和 Store，锁随事务结束释放。
func (store *Store) WithPurgeRootTransaction(ctx context.Context, candidate domain.PurgeRootCandidate, action func(context.Context, *Store) error) error {
	// 1. 校验候选，开启所属用户事务。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核永久清理根项需要数据入口、上下文和事务回调")
	}
	if !candidate.IsValid() {
		return domain.ErrInvalidFileEntryData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, candidate.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 先按归属定位，确认目标后再加排他锁读取当前状态。
		if _, err := txStore.FindUserFileEntry(txContext, candidate.UserID, candidate.EntryID); err != nil {
			if errors.Is(err, domain.ErrFileEntryNotFound) {
				return domain.ErrPurgeRootCandidateStale
			}
			return err
		}
		current, err := txStore.readLockedTrashEntry(txContext, candidate.UserID, candidate.EntryID)
		if errors.Is(err, domain.ErrFileEntryNotFound) {
			return domain.ErrPurgeRootCandidateStale
		}
		if err != nil {
			return err
		}

		// 3. 复核候选，通过后执行同一事务内的回调。
		switch current.Status {
		case "ACTIVE", "TRASHED", "PURGED":
			return domain.ErrPurgeRootCandidateStale
		case "PURGING":
		default:
			return domain.ErrInvalidFileEntryData
		}
		if current.ParentID == nil || current.PurgeStartedAt == nil {
			return domain.ErrInvalidFileEntryData
		}
		lockedCandidate := domain.PurgeRootCandidate{
			EntryID: current.ID, UserID: current.UserID, ParentID: *current.ParentID,
			Revision: current.Revision, PurgeStartedAt: *current.PurgeStartedAt,
		}
		if !lockedCandidate.IsValid() {
			return domain.ErrInvalidFileEntryData
		}
		if lockedCandidate.ParentID != candidate.ParentID || lockedCandidate.Revision != candidate.Revision ||
			!lockedCandidate.PurgeStartedAt.Equal(candidate.PurgeStartedAt) {
			return domain.ErrPurgeRootCandidateStale
		}
		purgeStore := *txStore
		purgeStore.purgeRoot = &lockedCandidate
		return action(txContext, &purgeStore)
	})
}

// requirePurgeRoot 检查 Store 是否来自已验证且持锁的根项事务。
func (store *Store) requirePurgeRoot(ctx context.Context) (domain.PurgeRootCandidate, error) {
	// 1. 核对根项绑定与事务连接。
	if store == nil || store.database == nil || ctx == nil || store.purgeRoot == nil {
		return domain.PurgeRootCandidate{}, fmt.Errorf("清理叶子操作需要已验证的根项事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return domain.PurgeRootCandidate{}, fmt.Errorf("清理叶子操作必须使用根项事务 Store")
	}
	root := *store.purgeRoot
	if root.UserID != store.loginUserID || !root.IsValid() {
		return domain.PurgeRootCandidate{}, domain.ErrInvalidFileEntryData
	}
	return root, ctx.Err()
}
