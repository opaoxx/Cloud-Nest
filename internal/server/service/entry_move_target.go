package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// EntryMoveTargetService 复用完整路径检查，校验源目录项和目标父目录的关系。
type EntryMoveTargetService struct {
	// withSnapshot 同步执行一次读取回调，可适配同一快照或当前用户事务。
	withSnapshot func(context.Context, func(context.Context, FileEntryPathReader) error) error
}

// NewEntryMoveTargetService 绑定同一读取范围内的路径能力。
func NewEntryMoveTargetService(withSnapshot func(context.Context, func(context.Context, FileEntryPathReader) error) error) *EntryMoveTargetService {
	// 1. 检查并保存路径读取入口。
	if withSnapshot == nil {
		panic("移动目标关系服务需要同一读取范围内的路径入口")
	}
	return &EntryMoveTargetService{withSnapshot: withSnapshot}
}

// Check 为已认证 Actor 检查源和目标父目录关系，成功返回独立路径。
// 后续移动服务继续在同一用户事务内完成版本、子树规模和移动后深度检查。
func (targetService *EntryMoveTargetService) Check(ctx context.Context, actor domain.Actor, entryID, targetParentID uint64) (*domain.EntryMovePaths, error) {
	// 1. 检查身份、源 ID、目标父目录 ID 和取消状态。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 || targetParentID == 0 {
		return nil, fmt.Errorf("检查移动关系需要上下文、完整认证身份和非零源及目标父目录 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 读取源路径，检查根目录移动限制。
	var prepared *domain.EntryMovePaths
	snapshotError := targetService.withSnapshot(ctx, func(snapshotContext context.Context, reader FileEntryPathReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("移动关系回调需要读取上下文和路径能力")
		}
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, reader)
		})
		source, err := paths.Get(snapshotContext, actor, entryID)
		if err != nil {
			return err
		}
		entry := source.Entries[len(source.Entries)-1]
		if entry.ParentID == nil {
			return domain.ErrRootEntryMove
		}

		// 3. 读取目标父目录路径，确认目录类型和共同根目录。
		target, err := paths.Get(snapshotContext, actor, targetParentID)
		if err != nil {
			return err
		}
		if target.Entries[len(target.Entries)-1].EntryType != "DIR" {
			return domain.ErrFileEntryNotDirectory
		}
		if source.Entries[0].ID != target.Entries[0].ID {
			return domain.ErrInvalidFileEntryData
		}

		// 4. 检查目标父链，拒绝移入自身或自身子树。
		for _, ancestor := range target.Entries {
			if ancestor.ID == entry.ID {
				return domain.ErrEntryMoveIntoSubtree
			}
		}
		prepared = &domain.EntryMovePaths{Source: *source, TargetParent: *target}
		return nil
	})

	// 5. 确认读取完成，返回已校验的独立路径。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("移动关系检查失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
