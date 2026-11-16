package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RenameEntryTransaction 汇集重命名使用的事务内路径读取与条件写入能力。
type RenameEntryTransaction interface {
	FileEntryPathReader
	// RenameUserFileEntry 按版本更新当前用户目录项，结果由外层事务确认提交。
	RenameUserFileEntry(ctx context.Context, entryID, expectedRevision uint64, metadata domain.EntryNameMetadata, nameKey []byte) (*domain.FileEntry, error)
}

// RenameEntryService 组合完整路径、命名规则、版本校验及已认证用户事务写入。
type RenameEntryService struct {
	// withTransaction 同步执行回调，传播失败，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, RenameEntryTransaction) error) error
}

// NewRenameEntryService 绑定已有已认证用户事务入口。
func NewRenameEntryService(withTransaction func(context.Context, domain.Actor, func(context.Context, RenameEntryTransaction) error) error) *RenameEntryService {
	// 1. 检查并保存重命名所需的事务方法。
	if withTransaction == nil {
		panic("重命名服务需要已认证用户事务方法")
	}
	return &RenameEntryService{withTransaction: withTransaction}
}

// Rename 为已认证 Actor 按预期版本重命名可见的普通文件或目录。
// 错误返回 nil，仅在外层事务确认提交后交付独立记录。
func (renameService *RenameEntryService) Rename(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64, rawName string) (*domain.FileEntry, error) {
	// 1. 检查身份、目标、预期版本和名称，生成名称键。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 || expectedRevision == 0 {
		return nil, fmt.Errorf("重命名需要上下文、完整认证身份、非零目标和预期版本")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := ValidateEntryName(rawName)
	if err != nil {
		return nil, err
	}
	nameKey, err := BuildEntryNameKey(name)
	if err != nil {
		return nil, err
	}

	// 2. 进入用户事务，检查目标及全部祖先的可见性。
	var prepared *domain.FileEntry
	transactionError := renameService.withTransaction(ctx, actor, func(txContext context.Context, txStore RenameEntryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("重命名回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, txStore)
		})
		path, err := paths.Get(txContext, actor, entryID)
		if err != nil {
			return err
		}

		// 3. 检查根目录限制和预期版本，计算名称派生字段。
		target := path.Entries[len(path.Entries)-1]
		if target.ParentID == nil {
			return domain.ErrRootEntryRename
		}
		if err := ValidateEntryRevision(target.Revision, expectedRevision); err != nil {
			return err
		}
		metadata, err := BuildEntryNameMetadata(target.EntryType, name)
		if err != nil {
			return err
		}

		// 4. 调用条件更新，核对并复制结果。
		resultRevision := expectedRevision
		if target.Name != name {
			if expectedRevision == ^uint64(0) {
				return domain.ErrEntryRevisionExhausted
			}
			resultRevision++
		}
		entry, renameError := txStore.RenameUserFileEntry(txContext, entryID, expectedRevision, *metadata, nameKey)
		if err := txContext.Err(); err != nil {
			return err
		}
		if renameError != nil {
			return fmt.Errorf("重命名目标目录项失败: %w", renameError)
		}
		if entry == nil || entry.ID != entryID || entry.UserID != actor.UserID || entry.ParentID == nil || *entry.ParentID != *target.ParentID ||
			entry.Name != name || entry.EntryType != target.EntryType || entry.FileSize != target.FileSize || entry.Status != "ACTIVE" || entry.Revision != resultRevision ||
			entry.CreatedAt.IsZero() || !entry.CreatedAt.Equal(target.CreatedAt) || entry.ModifiedAt.IsZero() || entry.UpdatedAt.IsZero() {
			return domain.ErrInvalidFileEntryData
		}
		if target.Name == name && (!entry.ModifiedAt.Equal(target.ModifiedAt) || !entry.UpdatedAt.Equal(target.UpdatedAt)) {
			return domain.ErrInvalidFileEntryData
		}
		entryCopy := *entry
		entryCopy.ParentID = copyOptionalValue(entry.ParentID)
		prepared = &entryCopy
		return nil
	})

	// 5. 确认事务提交成功，返回独立结果。
	if transactionError != nil {
		return nil, fmt.Errorf("重命名事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
