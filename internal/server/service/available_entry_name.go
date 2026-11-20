package service

import (
	"context"
	"errors"
	"math"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// resolveAvailableEntryName 检查原名及连续编号的候选，调用方先校验输入并锁定用户。
func resolveAvailableEntryName(
	ctx context.Context,
	userID, parentID uint64,
	originalName string,
	buildCandidate func(string, int) (string, error),
	findActiveEntry func(context.Context, uint64, uint64, []byte) (*domain.FileEntry, error),
) (string, error) {
	candidate := originalName
	for checkedNumber := 0; ; checkedNumber++ {
		// 2. 为当前候选生成名称键，在实际父目录中查询活动同名项。
		if err := ctx.Err(); err != nil {
			return "", err
		}
		nameKey, err := BuildEntryNameKey(candidate)
		if err != nil {
			return "", err
		}
		entry, lookupError := findActiveEntry(ctx, userID, parentID, nameKey)
		if err := ctx.Err(); err != nil {
			return "", err
		}

		// 3. 名称空闲就返回，否则从编号 1 开始依次检查。
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
		if checkedNumber == math.MaxInt {
			return "", domain.ErrEntryNameConflict
		}
		// 每个编号均基于原名，避免把上一轮编号带入下一轮。
		candidate, err = buildCandidate(originalName, checkedNumber+1)
		if err != nil {
			return "", err
		}
	}
}
