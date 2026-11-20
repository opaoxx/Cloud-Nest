package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ResolveRestoreEntryName 返回实际恢复父目录下第一个空闲的展示名。
// 调用方在同一用户事务中检查目标、选择父目录、查重并执行恢复写入。
func ResolveRestoreEntryName(
	ctx context.Context,
	target *domain.FileEntry,
	parentID uint64,
	findActiveEntry func(context.Context, uint64, uint64, []byte) (*domain.FileEntry, error),
) (string, error) {
	// 1. 核对回收项、实际父目录和原名称。
	if ctx == nil || parentID == 0 || findActiveEntry == nil {
		return "", fmt.Errorf("选择恢复名称需要事务上下文、实际父目录 ID 及同名查询")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if target == nil || target.ID == 0 || target.UserID == 0 || target.Revision == 0 || target.ID == parentID ||
		target.Status != "TRASHED" || (target.EntryType != "FILE" && target.EntryType != "DIR") {
		return "", domain.ErrInvalidFileEntryData
	}
	originalName, err := ValidateEntryName(target.Name)
	if err != nil || originalName != target.Name {
		return "", domain.ErrInvalidFileEntryData
	}

	// 2. 为当前候选生成名称键，在实际父目录中查询活动同名项。
	// 3. 名称空闲就返回，否则从编号 1 开始依次检查。
	entryType := target.EntryType
	return resolveAvailableEntryName(ctx, target.UserID, parentID, originalName,
		func(name string, suffixNumber int) (string, error) {
			return BuildRestoreConflictName(name, entryType, suffixNumber)
		}, findActiveEntry)
}
