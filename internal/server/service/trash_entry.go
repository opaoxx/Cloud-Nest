package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// TrashEntryTransaction 汇集回收所需的路径读取和条件写入。
type TrashEntryTransaction interface {
	FileEntryPathReader
	// TrashUserFileEntry 按版本回收当前用户的目录项，提交由外层事务负责。
	TrashUserFileEntry(ctx context.Context, entryID, expectedRevision uint64) (*domain.TrashedFileEntry, error)
}

// TrashEntryService 在同一用户事务内完成单项回收。
type TrashEntryService struct {
	// withTransaction 同步执行一次回调，确认提交后才返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, TrashEntryTransaction) error) error
}

// NewTrashEntryService 绑定已有的认证用户事务。
func NewTrashEntryService(withTransaction func(context.Context, domain.Actor, func(context.Context, TrashEntryTransaction) error) error) *TrashEntryService {
	// 1. 检查并保存回收事务入口。
	if withTransaction == nil {
		panic("回收服务需要已认证用户事务方法")
	}
	return &TrashEntryService{withTransaction: withTransaction}
}

// Trash 回收可见普通目录项并在事务提交后交付独立结果。
func (trashService *TrashEntryService) Trash(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64) (*domain.TrashedFileEntry, error) {
	// 1. 检查调用参数，进入已认证用户事务。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 ||
		entryID == 0 || expectedRevision == 0 {
		return nil, fmt.Errorf("回收需要上下文、完整认证身份、非零目标和预期版本")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var prepared *domain.TrashedFileEntry
	transactionError := trashService.withTransaction(ctx, actor, func(txContext context.Context, txStore TrashEntryTransaction) error {
		// 2. 在同一事务中检查完整路径并写入回收状态。
		target, err := ValidateTrashEntryTarget(txContext, actor, entryID, expectedRevision, txStore)
		if err != nil {
			return err
		}
		saved, writeError := txStore.TrashUserFileEntry(txContext, entryID, expectedRevision)
		if err := txContext.Err(); err != nil {
			return err
		}
		if writeError != nil {
			return fmt.Errorf("写入回收目录项失败: %w", writeError)
		}

		// 3. 核对目标、写入后状态和回收时间，准备独立结果。
		if saved == nil || saved.ID != target.ID || saved.UserID != target.UserID ||
			saved.ParentID == nil || *saved.ParentID != *target.ParentID || saved.Name != target.Name ||
			saved.EntryType != target.EntryType || saved.FileSize != target.FileSize ||
			saved.Status != "TRASHED" || saved.Revision != expectedRevision+1 ||
			saved.CreatedAt.IsZero() || !saved.CreatedAt.Equal(target.CreatedAt) {
			return domain.ErrInvalidFileEntryData
		}
		deletedAt := saved.DeletedAt.UTC()
		if deletedAt.Year() < 1000 || deletedAt.Year() > 9999 || deletedAt.Nanosecond()%1000 != 0 ||
			!saved.ModifiedAt.Equal(deletedAt) || !saved.UpdatedAt.Equal(deletedAt) {
			return domain.ErrInvalidFileEntryData
		}
		result := *saved
		result.ParentID = copyOptionalValue(saved.ParentID)
		result.DeletedAt = deletedAt
		prepared = &result
		return nil
	})

	// 4. 确认外层事务提交后交付结果。
	if transactionError != nil {
		return nil, fmt.Errorf("回收事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
