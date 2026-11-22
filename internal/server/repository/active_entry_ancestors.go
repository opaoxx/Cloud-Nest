package repository

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// lockedFileEntryStore 按账号与条目的复合索引执行当前读，保留原事务连接。
func (store *Store) lockedFileEntryStore(ctx context.Context) Store {
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).
		Table("file_entries FORCE INDEX (uk_entries_owner_id)").
		Clauses(clause.Locking{Strength: "UPDATE"})
	return lockedStore
}

// checkLockedActiveEntryAncestors 在所属账号事务内沿活动目录向上锁读。
func (store *Store) checkLockedActiveEntryAncestors(ctx context.Context, userID, parentID uint64) error {
	// 1. 在目录深度上限内，按账号归属沿父链查找根目录。
	lockedStore := store.lockedFileEntryStore(ctx)
	for depth := 1; depth <= domain.MaxFileEntryDepth; depth++ {
		parent, err := lockedStore.FindUserFileEntry(ctx, userID, parentID)
		if err != nil {
			return err
		}
		if parent.Status != "ACTIVE" || parent.EntryType != "DIR" {
			return domain.ErrFileEntryNotFound
		}
		if parent.ParentID == nil {
			return nil
		}
		parentID = *parent.ParentID
	}
	return domain.ErrFileEntryNotFound
}
