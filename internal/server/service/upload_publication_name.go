package service

import (
	"context"
	"errors"
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

	// 2. 先检查原名，冲突后按编号生成候选和名称键。
	candidate := originalName
	for suffixNumber := 1; ; suffixNumber++ {
		nameKey, err := BuildEntryNameKey(candidate)
		if err != nil {
			return "", err
		}

		// 3. 查询目标目录，遇到空闲名称即返回。
		entry, lookupError := findActiveEntry(ctx, userID, parentID, nameKey)
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if errors.Is(lookupError, domain.ErrFileEntryNotFound) {
			if entry != nil {
				return "", domain.ErrInvalidFileEntryData
			}
			return candidate, nil
		}
		if lookupError != nil {
			return "", lookupError
		}
		if entry == nil || entry.ID == 0 || entry.UserID != userID || entry.ParentID == nil ||
			*entry.ParentID != parentID || entry.Status != "ACTIVE" {
			return "", domain.ErrInvalidFileEntryData
		}
		candidate, err = BuildUploadConflictName(originalName, suffixNumber)
		if err != nil {
			return "", err
		}
	}
}
