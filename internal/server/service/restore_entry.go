package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RestoreEntryTransaction 提供同一事务内的恢复读取和写入能力。
type RestoreEntryTransaction interface {
	FileEntryPathReader
	// FindUserEntrySubtreeStats 读取目标自身和保留后代的统计。
	FindUserEntrySubtreeStats(ctx context.Context, userID, entryID uint64) (*domain.EntrySubtreeStats, error)
	// FindUserActiveFileEntryByName 按用户、父目录和名称键查找活动同名项。
	FindUserActiveFileEntryByName(ctx context.Context, userID, parentID uint64, nameKey []byte) (*domain.FileEntry, error)
	// RestoreUserFileEntry 按版本恢复当前用户的回收项。
	RestoreUserFileEntry(ctx context.Context, entryID, expectedRevision, parentID uint64, name string, nameKey []byte) (*domain.FileEntry, error)
}

// RestoreEntryService 在同一认证用户事务内完成单项恢复。
type RestoreEntryService struct {
	// withTransaction 同步执行一次回调，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, RestoreEntryTransaction) error) error
}

// NewRestoreEntryService 绑定认证用户事务入口。
func NewRestoreEntryService(withTransaction func(context.Context, domain.Actor, func(context.Context, RestoreEntryTransaction) error) error) *RestoreEntryService {
	// 1. 检查并保存恢复事务入口。
	if withTransaction == nil {
		panic("恢复服务需要已认证用户事务方法")
	}
	return &RestoreEntryService{withTransaction: withTransaction}
}

// Restore 按预期版本恢复普通目录项，提交成功后返回独立结果。
func (restoreService *RestoreEntryService) Restore(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64) (*domain.FileEntry, error) {
	// 1. 校验输入并进入已认证用户事务。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 || expectedRevision == 0 {
		return nil, fmt.Errorf("恢复需要上下文、完整认证身份、非零目标和预期版本")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var prepared *domain.FileEntry
	transactionError := restoreService.withTransaction(ctx, actor, func(txContext context.Context, txStore RestoreEntryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("恢复回调需要事务上下文和 Store")
		}

		// 2. 检查回收目标及原父链，再比对预期版本。
		target, err := ReadRestoreEntryTarget(txContext, actor, entryID, txStore)
		if err != nil {
			return err
		}
		ancestors, err := ReadRestoreEntryAncestors(txContext, actor, target, txStore)
		if err != nil {
			return err
		}
		if err := ValidateRestoreEntryRevision(target.Revision, expectedRevision); err != nil {
			return err
		}

		// 3. 读取子树深度，选出实际父目录和可用名称。
		stats, statsError := txStore.FindUserEntrySubtreeStats(txContext, actor.UserID, entryID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if statsError != nil {
			return fmt.Errorf("读取恢复子树统计失败: %w", statsError)
		}
		parentID, err := SelectRestoreEntryParent(target, ancestors, stats)
		if err != nil {
			return err
		}
		name, err := ResolveRestoreEntryName(txContext, target, parentID, txStore.FindUserActiveFileEntryByName)
		if err != nil {
			return err
		}
		nameKey, err := BuildEntryNameKey(name)
		if err != nil {
			return err
		}
		if err := txContext.Err(); err != nil {
			return err
		}

		// 4. 按版本写入，核对返回的目录项。
		saved, writeError := txStore.RestoreUserFileEntry(txContext, entryID, expectedRevision, parentID, name, nameKey)
		if err := txContext.Err(); err != nil {
			return err
		}
		if writeError != nil {
			return fmt.Errorf("写入恢复目录项失败: %w", writeError)
		}
		if saved == nil || saved.ID != target.ID || saved.UserID != actor.UserID ||
			saved.ParentID == nil || *saved.ParentID != parentID || saved.Name != name ||
			saved.Status != "ACTIVE" || saved.Revision != expectedRevision+1 ||
			saved.EntryType != target.EntryType || saved.FileSize != target.FileSize ||
			saved.CreatedAt.IsZero() || !saved.CreatedAt.Equal(target.CreatedAt) ||
			saved.ModifiedAt.IsZero() || !saved.ModifiedAt.Equal(saved.UpdatedAt) || saved.ModifiedAt.Nanosecond()%1000 != 0 {
			return domain.ErrInvalidFileEntryData
		}
		result := *saved
		result.ParentID = copyOptionalValue(saved.ParentID)
		prepared = &result
		return nil
	})

	// 5. 确认事务提交，再交付结果。
	if transactionError != nil {
		return nil, fmt.Errorf("恢复事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
