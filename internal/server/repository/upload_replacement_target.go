package repository

import (
	"context"
	"fmt"
	"math"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// LockUploadReplacementTarget 核对所属账号事务内的活动文件、可见路径和预期版本。
// 发布时参数取自任务绑定。文件和祖先的锁由外层事务持有，本方法只读取。
func (store *Store) LockUploadReplacementTarget(ctx context.Context, entryID, expectedRevision uint64) (*domain.FileEntry, error) {
	// 1. 要求持锁账号事务，按该账号锁读目标。
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 || entryID == 0 || expectedRevision == 0 {
		return nil, fmt.Errorf("复核替换目标需要账号事务、文件 ID 和预期版本")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("复核替换目标必须使用账号事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	userID := store.loginUserID
	lockedStore := store.lockedFileEntryStore(ctx)
	entry, err := lockedStore.FindUserFileEntry(ctx, userID, entryID)
	if err != nil {
		return nil, err
	}

	// 2. 确认可见路径，再区分目录和文件。隐藏目标统一按未找到处理。
	if entry.Status != "ACTIVE" {
		return nil, domain.ErrFileEntryNotFound
	}
	if entry.ParentID != nil {
		if err := store.checkLockedActiveEntryAncestors(ctx, userID, *entry.ParentID); err != nil {
			return nil, err
		}
	}
	if entry.EntryType != "FILE" {
		return nil, domain.ErrFileEntryNotFile
	}
	if entry.ParentID == nil {
		return nil, domain.ErrFileEntryNotFound
	}

	// 3. 只在目标可见后比较版本，拒绝冲突及无法递增的版本。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entry.Revision != expectedRevision {
		return nil, &domain.EntryRevisionConflictError{CurrentRevision: entry.Revision}
	}
	if entry.Revision == math.MaxUint64 {
		return nil, domain.ErrEntryRevisionExhausted
	}
	return entry, nil
}
