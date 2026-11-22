package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateFileReuseTarget 校验同账号秒传的目标目录和新增文件深度。
// actor 来自认证，reader 由调用方在同一账号事务内提供。
func ValidateFileReuseTarget(ctx context.Context, actor domain.Actor, parentID uint64, reader FileEntryPathReader) (*domain.FileEntryPath, error) {
	// 1. 核对认证身份、事务读取能力和目标 ID。
	if ctx == nil || reader == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 || parentID == 0 {
		return nil, fmt.Errorf("校验秒传目标需要上下文、认证身份、读取能力和非零目录 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 读取目标至当前账号根目录的完整活动路径。
	path, err := readFileEntryPath(ctx, reader, actor.UserID, parentID)
	if stopError := ctx.Err(); stopError != nil {
		return nil, stopError
	}
	if err != nil {
		return nil, err
	}

	// 3. 确认目标是目录，且新增文件后仍在深度上限内。
	if err := validateFileCreationParent(path); err != nil {
		return nil, err
	}
	return path, nil
}
