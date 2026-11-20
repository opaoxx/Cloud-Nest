package service

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RestoreEntryAncestors 提供后续选择恢复位置所需的原父链信息。
type RestoreEntryAncestors struct {
	RootEntryID         uint64
	OriginalParentDepth uint64 // 真实根目录深度为 0。
	HasTrashedAncestor  bool
}

// ReadRestoreEntryAncestors 检查原父目录到真实根目录的链路。
// target 来自同一已认证用户事务内的 ReadRestoreEntryTarget。
func ReadRestoreEntryAncestors(ctx context.Context, actor domain.Actor, target *domain.FileEntry, reader FileEntryPathReader) (*RestoreEntryAncestors, error) {
	// 1. 核对目标归属，读出当前用户的真实根目录。
	if ctx == nil || reader == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, fmt.Errorf("检查恢复原父链需要事务上下文、读取能力和完整认证身份")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if target == nil || target.ID == 0 || target.UserID != actor.UserID || target.Revision == 0 ||
		target.Status != "TRASHED" || target.ParentID == nil || *target.ParentID == 0 || *target.ParentID == target.ID {
		return nil, domain.ErrInvalidFileEntryData
	}
	parentID := *target.ParentID
	profile, err := NewUserProfileService(reader.FindUserProfile).Get(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	if target.ID == profile.RootEntryID {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 从原父目录逐级向上，核对目录结构并记录祖先状态。
	visited := map[uint64]bool{target.ID: true}
	hasTrashedAncestor, hasCleanupAncestor := false, false
	parentDepth := uint64(0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// 目标自身占一层，最多向上读取 32 个祖先（含根目录）。
		if parentDepth >= domain.MaxFileEntryDepth || visited[parentID] {
			return nil, domain.ErrInvalidFileEntryData
		}
		visited[parentID] = true
		parent, queryError := reader.FindUserFileEntry(ctx, actor.UserID, parentID)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if errors.Is(queryError, domain.ErrFileEntryNotFound) {
			return nil, domain.ErrInvalidFileEntryData // 外键关联的父项应存在。
		}
		if queryError != nil {
			return nil, fmt.Errorf("读取恢复原父链失败: %w", queryError)
		}
		if parent == nil || parent.ID != parentID || parent.UserID != actor.UserID || parent.Revision == 0 ||
			parent.EntryType != "DIR" || parent.FileSize != 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
		if parentID == profile.RootEntryID {
			if parent.ParentID != nil || parent.Name != "" || parent.Status != "ACTIVE" {
				return nil, domain.ErrInvalidFileEntryData
			}
			break
		}
		if parent.ParentID == nil || *parent.ParentID == 0 || parent.Name == "" || !utf8.ValidString(parent.Name) {
			return nil, domain.ErrInvalidFileEntryData
		}
		switch parent.Status {
		case "ACTIVE":
		case "TRASHED":
			hasTrashedAncestor = true
		case "PURGING", "PURGED":
			hasCleanupAncestor = true
		default:
			return nil, domain.ErrInvalidFileEntryData
		}
		parentID = *parent.ParentID
		parentDepth++
	}

	// 3. 到达真实根目录后，拒绝带有永久清理祖先的目标，或交付父链信息。
	if hasCleanupAncestor {
		return nil, domain.ErrFileEntryNotFound
	}
	return &RestoreEntryAncestors{
		RootEntryID:         profile.RootEntryID,
		OriginalParentDepth: parentDepth,
		HasTrashedAncestor:  hasTrashedAncestor,
	}, nil
}
