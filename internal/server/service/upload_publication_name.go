package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ResolveUploadPublicationName 返回目标目录中第一个空闲的上传展示名。
// 调用方先检查目标路径，并在持有用户锁的同一事务中查询名称和创建目录项。
func ResolveUploadPublicationName(
	ctx context.Context,
	userID, parentID uint64,
	requestedName string,
	findActiveEntry func(context.Context, uint64, uint64, []byte) (*domain.FileEntry, error),
) (string, error) {
	// 1. 检查查询参数和原文件名。
	if ctx == nil || userID == 0 || parentID == 0 || findActiveEntry == nil {
		return "", fmt.Errorf("选择上传发布名称需要事务上下文、用户与父目录 ID 及同名查询")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	originalName, err := ValidateEntryName(requestedName)
	if err != nil {
		return "", err
	}

	return resolveAvailableEntryName(ctx, userID, parentID, originalName, BuildUploadConflictName, findActiveEntry)
}
