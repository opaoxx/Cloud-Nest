package service

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CheckTrashEntryVisibility 返回回收候选的原父路径。
// candidate 和 reader 来自同一只读快照。路径从真实根目录到原父目录，隐藏时返回 nil, nil。
func CheckTrashEntryVisibility(ctx context.Context, actor domain.Actor, candidate domain.TrashedFileEntry, reader FileEntryPathReader) (*domain.FileEntryPath, error) {
	// 1. 核对认证身份、回收候选和原父目录 ID。
	if ctx == nil || reader == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 {
		return nil, fmt.Errorf("检查回收候选可见性需要快照上下文、路径读取能力和完整认证身份")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	position := domain.TrashEntryPosition{DeletedAt: candidate.DeletedAt, ID: candidate.ID}
	if candidate.UserID != actor.UserID || candidate.Status != "TRASHED" || !position.IsValid() ||
		candidate.ParentID == nil || *candidate.ParentID == 0 || *candidate.ParentID == candidate.ID ||
		candidate.Revision == 0 || candidate.Name == "" || !utf8.ValidString(candidate.Name) ||
		(candidate.EntryType != "DIR" && candidate.EntryType != "FILE") ||
		(candidate.EntryType == "DIR" && candidate.FileSize != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 沿原父目录检查活动路径，把候选本身计入深度。
	path, err := readActiveFileEntryPath(ctx, reader, actor.UserID, *candidate.ParentID, candidate.ID)

	// 3. 隐藏有回收或清理中祖先的候选，交付通过检查的原父路径。
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	// 仅活动路径检查直接给出的不可见标记表示隐藏。
	if err == domain.ErrFileEntryNotFound {
		return nil, nil
	}
	return path, err
}
