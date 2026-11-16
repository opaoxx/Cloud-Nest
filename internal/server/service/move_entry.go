package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MoveEntryTransaction 汇集移动使用的事务内路径、子树统计和条件写入能力。
type MoveEntryTransaction interface {
	FileEntryPathReader
	// FindUserEntrySubtreeStats 统计包含源自身的全部保留后代。
	FindUserEntrySubtreeStats(ctx context.Context, userID, entryID uint64) (*domain.EntrySubtreeStats, error)
	// MoveUserFileEntry 按版本更新位置，结果由外层事务确认提交。
	MoveUserFileEntry(ctx context.Context, entryID, expectedRevision, targetParentID uint64) (*domain.FileEntry, error)
}

// MoveEntryService 在同一已认证用户事务内组合移动规则及写入。
type MoveEntryService struct {
	// withTransaction 同步执行回调，传播失败，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, MoveEntryTransaction) error) error
}

// NewMoveEntryService 绑定已有已认证用户事务入口。
func NewMoveEntryService(withTransaction func(context.Context, domain.Actor, func(context.Context, MoveEntryTransaction) error) error) *MoveEntryService {
	// 1. 检查并保存移动所需的事务方法。
	if withTransaction == nil {
		panic("移动服务需要已认证用户事务方法")
	}
	return &MoveEntryService{withTransaction: withTransaction}
}

// Move 按预期版本移动可见的普通目录项，外层确认提交后返回独立基础记录。
func (moveService *MoveEntryService) Move(ctx context.Context, actor domain.Actor, entryID, expectedRevision, targetParentID uint64) (*domain.FileEntry, error) {
	// 1. 检查身份、源、预期版本、目标父目录和取消状态。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 || expectedRevision == 0 || targetParentID == 0 {
		return nil, fmt.Errorf("移动需要上下文、完整认证身份、非零源/预期版本/目标父目录")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入用户事务，校验源与目标的完整路径及关系。
	var prepared *domain.FileEntry
	transactionError := moveService.withTransaction(ctx, actor, func(txContext context.Context, txStore MoveEntryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("移动回调需要事务上下文和 Store")
		}
		checker := NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, txStore)
		})
		paths, err := checker.Check(txContext, actor, entryID, targetParentID)
		if err != nil {
			return err
		}

		// 3. 校验源版本，读取子树统计并检查规模和最终深度。
		source := paths.Source.Entries[len(paths.Source.Entries)-1]
		if err := ValidateEntryRevision(source.Revision, expectedRevision); err != nil {
			return err
		}
		stats, statsError := txStore.FindUserEntrySubtreeStats(txContext, actor.UserID, entryID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if statsError != nil {
			return fmt.Errorf("读取移动子树统计失败: %w", statsError)
		}
		if err := ValidateEntryMoveLimits(paths, stats); err != nil {
			return err
		}

		// 4. 按版本移动，核对结果并复制父目录指针。
		resultRevision := expectedRevision
		if *source.ParentID != targetParentID {
			if expectedRevision == ^uint64(0) {
				return domain.ErrEntryRevisionExhausted
			}
			resultRevision++
		}
		entry, moveError := txStore.MoveUserFileEntry(txContext, entryID, expectedRevision, targetParentID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if moveError != nil {
			return fmt.Errorf("移动目录项失败: %w", moveError)
		}
		if entry == nil || entry.ID != entryID || entry.UserID != actor.UserID || entry.ParentID == nil || *entry.ParentID != targetParentID ||
			entry.Name != source.Name || entry.EntryType != source.EntryType || entry.FileSize != source.FileSize || entry.Status != "ACTIVE" || entry.Revision != resultRevision ||
			entry.CreatedAt.IsZero() || !entry.CreatedAt.Equal(source.CreatedAt) || entry.ModifiedAt.IsZero() || entry.UpdatedAt.IsZero() {
			return domain.ErrInvalidFileEntryData
		}
		if *source.ParentID == targetParentID && (!entry.ModifiedAt.Equal(source.ModifiedAt) || !entry.UpdatedAt.Equal(source.UpdatedAt)) {
			return domain.ErrInvalidFileEntryData
		}
		entryCopy := *entry
		entryCopy.ParentID = copyOptionalValue(entry.ParentID)
		prepared = &entryCopy
		return nil
	})

	// 5. 确认外层事务提交成功，返回独立结果。
	if transactionError != nil {
		return nil, fmt.Errorf("移动事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
