package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// FileEntryVisibilityReader 提供当前只读快照中的资料和批量基础记录读取。
type FileEntryVisibilityReader interface {
	// FindUserProfile 提供当前用户状态和真实根目录。
	FindUserProfile(context.Context, uint64) (*domain.UserProfile, error)
	// FindUserFileEntries 按当前用户取得一批基础记录及生命周期原值。
	FindUserFileEntries(context.Context, uint64, []uint64) (map[uint64]domain.FileEntry, error)
}

// FilterVisibleFileEntries 筛选至多一千项普通候选，成功结果按输入顺序排列。
// reader 和 candidates 应由调用方在同一只读快照中取得，事务结束由调用方确认。
// 隐藏路径被过滤，异常父链或读取失败返回 nil 与错误，空结果使用初始化切片。
func FilterVisibleFileEntries(ctx context.Context, actor domain.Actor, candidates []domain.FileEntry, reader FileEntryVisibilityReader) ([]domain.FileEntry, error) {
	// 1. 检查身份与候选范围，复制候选记录。
	if ctx == nil || reader == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || len(candidates) > domain.MaxGlobalEntryCandidates {
		return nil, fmt.Errorf("批量可见性检查需要上下文、完整身份、快照读取能力和至多一千项候选")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prepared := copyFileEntryRecords(candidates)
	requestedIDs := make(map[uint64]bool, len(prepared))
	for _, entry := range prepared {
		if entry.ID == 0 || entry.UserID != actor.UserID || entry.Revision == 0 || entry.ParentID == nil || *entry.ParentID == 0 || requestedIDs[entry.ID] {
			return nil, domain.ErrInvalidFileEntryData
		}
		requestedIDs[entry.ID] = true
	}

	// 2. 复查当前用户资料，准备请求内缓存。
	profile, err := NewUserProfileService(reader.FindUserProfile).Get(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	cached := &visibleEntryPathReader{profile: *profile, entries: make(map[uint64]domain.FileEntry, len(prepared))}
	var pending []uint64
	for _, entry := range prepared {
		cached.entries[entry.ID] = entry
		if entry.Status == "ACTIVE" && !requestedIDs[*entry.ParentID] {
			requestedIDs[*entry.ParentID] = true
			pending = append(pending, *entry.ParentID)
		}
	}

	// 3. 逐层批量补齐祖先，复用当前快照读取。
	for depth := 0; depth < domain.MaxFileEntryDepth && len(pending) > 0; depth++ {
		batchIDs := make(map[uint64]bool, len(pending))
		for _, entryID := range pending {
			batchIDs[entryID] = true
		}
		entries, readError := reader.FindUserFileEntries(ctx, actor.UserID, slices.Clone(pending))
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if readError != nil {
			return nil, fmt.Errorf("批量读取候选祖先失败: %w", readError)
		}
		if entries == nil {
			return nil, domain.ErrInvalidFileEntryData
		}
		for entryID, entry := range entries {
			if !batchIDs[entryID] || entry.ID != entryID || entry.UserID != actor.UserID || entry.Revision == 0 || (entry.ParentID != nil && *entry.ParentID == 0) {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
		next := make([]uint64, 0, len(entries))
		for _, entryID := range pending {
			entry, found := entries[entryID]
			if !found {
				continue
			}
			entry.ParentID = copyOptionalValue(entry.ParentID)
			cached.entries[entryID] = entry
			if entry.Status == "ACTIVE" && entry.EntryType == "DIR" && entry.ParentID != nil && !requestedIDs[*entry.ParentID] {
				requestedIDs[*entry.ParentID] = true
				next = append(next, *entry.ParentID)
			}
		}
		pending = next
	}

	// 4. 通过缓存复用既有路径检查，按原顺序保留可见项。
	paths := NewFileEntryPathService(func(pathContext context.Context, action func(context.Context, FileEntryPathReader) error) error {
		return action(pathContext, cached)
	})
	visible := make([]domain.FileEntry, 0, len(prepared))
	for _, entry := range prepared {
		path, err := paths.Get(ctx, actor, entry.ID)
		if errors.Is(err, domain.ErrFileEntryNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		visible = append(visible, path.Entries[len(path.Entries)-1])
	}

	// 5. 复查取消，交付独立结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return visible, nil
}

// visibleEntryPathReader 将请求内记录适配为已有路径服务的读取接口。
type visibleEntryPathReader struct {
	// profile 保存本次已复查的当前用户及真实根目录。
	profile domain.UserProfile
	// entries 保存已复制的候选及祖先，缺省键表达本次快照中未取得记录。
	entries map[uint64]domain.FileEntry
}

func (reader *visibleEntryPathReader) FindUserProfile(ctx context.Context, userID uint64) (*domain.UserProfile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if userID != reader.profile.ID {
		return nil, domain.ErrAccountNotFound
	}
	profile := reader.profile
	return &profile, nil
}

func (reader *visibleEntryPathReader) FindUserFileEntry(ctx context.Context, userID, entryID uint64) (*domain.FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry, found := reader.entries[entryID]
	if !found || entry.UserID != userID {
		return nil, domain.ErrFileEntryNotFound
	}
	entry.ParentID = copyOptionalValue(entry.ParentID)
	return &entry, nil
}
