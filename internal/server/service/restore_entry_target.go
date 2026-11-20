package service

import (
	"context"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ReadRestoreEntryTarget 读取可进入原父链检查的回收目标。
// 调用方使用同一已认证用户事务的上下文和 reader，并在比较版本前检查原父链。
func ReadRestoreEntryTarget(ctx context.Context, actor domain.Actor, entryID uint64, reader FileEntryPathReader) (*domain.FileEntry, error) {
	// 1. 核对身份和目标 ID，按当前用户读取目录项。
	if ctx == nil || reader == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 {
		return nil, fmt.Errorf("读取恢复目标需要事务上下文、读取能力、完整认证身份及非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry, queryError := reader.FindUserFileEntry(ctx, actor.UserID, entryID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取待恢复目录项失败: %w", queryError)
	}
	if entry == nil || entry.ID != entryID || entry.UserID != actor.UserID || entry.Revision == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 确认真正的根目录，检查目标是否仍为 TRASHED。
	profile, err := NewUserProfileService(reader.FindUserProfile).Get(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	if entry.ID == profile.RootEntryID {
		if entry.ParentID != nil || entry.Name != "" || entry.EntryType != "DIR" || entry.Status != "ACTIVE" || entry.FileSize != 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
		return nil, domain.ErrRootEntryRestore
	}
	if entry.ParentID == nil || *entry.ParentID == 0 || *entry.ParentID == entry.ID ||
		entry.Name == "" || !utf8.ValidString(entry.Name) ||
		(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	switch entry.Status {
	case "ACTIVE", "PURGING", "PURGED":
		return nil, domain.ErrFileEntryNotFound
	case "TRASHED":
		target := *entry
		target.ParentID = copyOptionalValue(entry.ParentID)
		return &target, nil
	default:
		return nil, domain.ErrInvalidFileEntryData
	}
}

// ValidateRestoreEntryRevision 检查预期版本及递增空间。
// 调用方先在同一用户事务中检查目标和原父链。
func ValidateRestoreEntryRevision(currentRevision, expectedRevision uint64) error {
	// 3. 复用版本比对，确认版本还能递增。
	if err := ValidateEntryRevision(currentRevision, expectedRevision); err != nil {
		return err
	}
	if currentRevision == math.MaxUint64 {
		return domain.ErrEntryRevisionExhausted
	}
	return nil
}
