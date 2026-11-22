package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// WithUserSyncSnapshot 在同一只读事务中取得快照头部并执行读取回调。
// userID 须来自认证。回调仅在执行期间使用传入的上下文与 Store。
func (store *Store) WithUserSyncSnapshot(ctx context.Context, userID uint64, action func(context.Context, *Store, domain.SyncSnapshotHeader) error) error {
	// 1. 校验用户 ID 和回调，复用五秒只读可重复读事务。
	if userID == 0 || action == nil {
		return fmt.Errorf("同步快照需要非零用户 ID 和读取回调")
	}
	return store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
		// 2. 通过首次普通查询取得根目录和截止序号，同时建立读取视图。
		header, err := reader.findUserSyncSnapshotHeader(readContext, userID)
		if err != nil {
			return err
		}

		// 3. 让后续读取继续使用同一事务，统一由外层入口结束事务。
		return action(readContext, reader, header)
	})
}

func (store *Store) findUserSyncSnapshotHeader(ctx context.Context, userID uint64) (domain.SyncSnapshotHeader, error) {
	// 1. 一次查询取得账号状态、变更序号和真实活动根目录。
	// LEFT JOIN 保留缺少根目录的账号，便于区分账号不存在和目录数据异常。
	var record struct {
		Status      string
		ChangeSeq   uint64
		RootEntryID *uint64
	}
	queryError := store.database.WithContext(ctx).
		Table("users AS accounts").
		Select("accounts.status, accounts.change_seq, roots.id AS root_entry_id").
		Joins("LEFT JOIN file_entries AS roots ON roots.root_owner_id = accounts.id "+
			"AND roots.user_id = accounts.id AND roots.parent_id IS NULL "+
			"AND roots.entry_type = ? AND roots.status = ?", "DIR", "ACTIVE").
		Where("accounts.id = ?", userID).
		Take(&record).Error

	// 2. 区分读取取消、缺少账号及数据库故障。
	if err := ctx.Err(); err != nil {
		return domain.SyncSnapshotHeader{}, fmt.Errorf("读取同步快照头部已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return domain.SyncSnapshotHeader{}, domain.ErrAccountNotFound
	}
	if queryError != nil {
		return domain.SyncSnapshotHeader{}, fmt.Errorf("读取同步快照头部失败: %w", queryError)
	}

	// 3. 仅向活动且具有真实根目录的账号返回快照头部，截止序号允许为零。
	if record.Status == "DISABLED" {
		return domain.SyncSnapshotHeader{}, domain.ErrAccountDisabled
	}
	if record.Status != "ACTIVE" || record.RootEntryID == nil || *record.RootEntryID == 0 {
		return domain.SyncSnapshotHeader{}, domain.ErrInvalidAccountData
	}
	return domain.SyncSnapshotHeader{
		RootEntryID:       *record.RootEntryID,
		HighWaterSequence: record.ChangeSeq,
	}, nil
}
