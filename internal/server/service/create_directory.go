package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateDirectoryTransaction 汇集目录创建使用的事务内路径读取与写入能力。
type CreateDirectoryTransaction interface {
	FileEntryPathReader
	// CreateUserDirectory 写入当前用户目录，结果在外层事务提交后确认生效。
	CreateUserDirectory(ctx context.Context, parentID uint64, name string, nameKey []byte, colorKey *string) (*domain.FileEntry, error)
}

// CreateDirectoryService 组合命名规则、父目录路径检查及已认证用户事务写入。
type CreateDirectoryService struct {
	// withTransaction 同步执行回调，传播失败，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, CreateDirectoryTransaction) error) error
}

// NewCreateDirectoryService 绑定已有已认证用户事务入口，缺少依赖属于装配错误。
func NewCreateDirectoryService(withTransaction func(context.Context, domain.Actor, func(context.Context, CreateDirectoryTransaction) error) error) *CreateDirectoryService {
	// 1. 检查并保存创建目录所需的事务方法。
	if withTransaction == nil {
		panic("创建文件夹服务需要已认证用户事务方法")
	}
	return &CreateDirectoryService{withTransaction: withTransaction}
}

// Create 为已认证 Actor 在可见父目录下创建普通文件夹。
// 名称保留去空白后的展示写法，错误返回 nil，仅在事务确认提交后交付独立记录。
func (createService *CreateDirectoryService) Create(ctx context.Context, actor domain.Actor, parentID uint64, rawName string, colorKey *string) (*domain.FileEntry, error) {
	// 1. 检查身份、输入和取消状态，生成展示名与名称键。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || parentID == 0 {
		return nil, fmt.Errorf("创建文件夹需要上下文、完整认证身份和非零父目录 ID")
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
	if !domain.ValidDirectoryColorKey(colorKey) {
		return nil, domain.ErrInvalidDirectoryColorKey
	}
	colorCopy := copyOptionalValue(colorKey)

	// 2. 进入用户事务，复用路径检查确认父目录可用。
	var prepared *domain.FileEntry
	transactionError := createService.withTransaction(ctx, actor, func(txContext context.Context, txStore CreateDirectoryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("创建文件夹回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, txStore)
		})
		path, err := paths.Get(txContext, actor, parentID)
		if err != nil {
			return err
		}

		// 3. 检查父目录类型及新目录的深度。
		if path.Entries[len(path.Entries)-1].EntryType != "DIR" {
			return domain.ErrFileEntryNotDirectory
		}
		if len(path.Entries) > domain.MaxFileEntryDepth {
			return domain.ErrFileEntryDepthExceeded
		}

		// 4. 写入目录，核对并复制事务内结果。
		entry, createError := txStore.CreateUserDirectory(txContext, parentID, name, nameKey, colorCopy)
		if err := txContext.Err(); err != nil {
			return err
		}
		if createError != nil {
			return fmt.Errorf("创建目标文件夹失败: %w", createError)
		}
		if entry == nil || entry.ID == 0 || entry.UserID != actor.UserID || entry.ParentID == nil || *entry.ParentID != parentID ||
			entry.Name != name || entry.EntryType != "DIR" || entry.Status != "ACTIVE" || entry.FileSize != 0 || entry.Revision != 1 ||
			entry.CreatedAt.IsZero() || entry.ModifiedAt.IsZero() || entry.UpdatedAt.IsZero() {
			return domain.ErrInvalidFileEntryData
		}
		for _, ancestor := range path.Entries {
			if entry.ID == ancestor.ID {
				return domain.ErrInvalidFileEntryData
			}
		}
		entryCopy := *entry
		parentCopy := *entry.ParentID
		entryCopy.ParentID = &parentCopy
		prepared = &entryCopy
		return nil
	})

	// 5. 确认事务提交成功，返回独立结果。
	if transactionError != nil {
		return nil, fmt.Errorf("创建文件夹事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
