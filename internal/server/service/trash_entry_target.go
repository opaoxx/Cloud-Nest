package service

import (
	"context"
	"fmt"
	"math"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateTrashEntryTarget 检查可见普通目录项的回收条件。
// 调用方在同一已认证用户事务内提供读取能力，并继续完成条件写入。
func ValidateTrashEntryTarget(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64, reader FileEntryPathReader) (*domain.FileEntry, error) {
	// 1. 校验身份、目标 ID 和预期版本。
	if ctx == nil || reader == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 ||
		entryID == 0 || expectedRevision == 0 {
		return nil, fmt.Errorf("检查回收目标需要事务上下文、路径读取能力、完整认证身份及非零目标和预期版本")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 读取完整活动路径，拒绝真实根目录。
	path, pathError := readFileEntryPath(ctx, reader, actor.UserID, entryID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pathError != nil {
		return nil, pathError
	}
	entry := path.Entries[len(path.Entries)-1]
	if entry.ParentID == nil {
		return nil, domain.ErrRootEntryTrash
	}

	// 3. 核对当前版本及递增空间，返回目标记录。
	if err := ValidateEntryRevision(entry.Revision, expectedRevision); err != nil {
		return nil, err
	}
	if entry.Revision == math.MaxUint64 {
		return nil, domain.ErrEntryRevisionExhausted
	}
	return &entry, nil
}
