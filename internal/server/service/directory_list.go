package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// DirectoryListReader 汇集同一快照内的目录路径和子项读取能力。
type DirectoryListReader interface {
	FileEntryPathReader
	// ListUserDirectoryEntries 按用户与父目录读取活动子项的一页。
	ListUserDirectoryEntries(ctx context.Context, userID, parentID uint64, before *domain.FileEntryPosition, pageSize int) (*domain.FileEntryPage, error)
}

// DirectoryListService 组合父目录可见性与同快照子项分页。
type DirectoryListService struct {
	// withSnapshot 同步执行一次回调，快照成功结束后返回 nil。
	withSnapshot func(context.Context, func(context.Context, DirectoryListReader) error) error
}

// NewDirectoryListService 绑定已有只读快照入口，缺少依赖属于装配错误。
func NewDirectoryListService(withSnapshot func(context.Context, func(context.Context, DirectoryListReader) error) error) *DirectoryListService {
	// 1. 检查并保存目录列表使用的快照方法。
	if withSnapshot == nil {
		panic("目录列表服务需要只读快照方法")
	}
	return &DirectoryListService{withSnapshot: withSnapshot}
}

// List 为已认证 Actor 读取可见目录的一页活动子项。
// 返回的路径与子项来自同一快照，失败或取消时返回空结果。
func (listService *DirectoryListService) List(ctx context.Context, actor domain.Actor, parentID uint64, before *domain.FileEntryPosition, pageSize int) (*domain.DirectoryList, error) {
	// 1. 检查认证身份、父目录、分页参数和上下文。
	if ctx == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 ||
		parentID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("读取目录列表需要上下文、完整认证身份、非零父目录 ID 和 1～100 的页大小")
	}
	var start domain.FileEntryPosition
	if before != nil {
		start = *before
		if !start.IsValid() {
			return nil, fmt.Errorf("目录列表翻页位置需要非零 ID 和数据库微秒时间")
		}
		start.ModifiedAt = start.ModifiedAt.UTC()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入只读快照，复用路径服务检查父目录可见性及类型。
	var prepared *domain.DirectoryList
	snapshotError := listService.withSnapshot(ctx, func(snapshotContext context.Context, reader DirectoryListReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("目录列表回调需要快照上下文和读取能力")
		}
		// 路径适配器直接在当前快照中执行已有路径检查回调。
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, reader)
		})
		path, err := paths.Get(snapshotContext, actor, parentID)
		if err != nil {
			return err
		}
		if path.Entries[len(path.Entries)-1].EntryType != "DIR" {
			return domain.ErrFileEntryNotDirectory
		}

		// 3. 使用同一快照读取活动子项，核对分页结果与范围。
		var queryBefore *domain.FileEntryPosition
		if before != nil {
			positionCopy := start
			queryBefore = &positionCopy
		}
		page, queryError := reader.ListUserDirectoryEntries(snapshotContext, actor.UserID, parentID, queryBefore, pageSize)
		if err := snapshotContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return fmt.Errorf("读取目录子项失败: %w", queryError)
		}
		if page == nil || len(page.Entries) > pageSize || (page.HasMore && len(page.Entries) != pageSize) {
			return domain.ErrInvalidFileEntryData
		}
		if len(path.Entries) > domain.MaxFileEntryDepth && len(page.Entries) > 0 {
			return domain.ErrInvalidFileEntryData
		}

		// 4. 复制路径和子项结果，快照成功结束后返回目录列表。
		entries := make([]domain.FileEntry, 0, len(page.Entries))
		seenIDs := make(map[uint64]bool, len(path.Entries)+len(page.Entries))
		for _, ancestor := range path.Entries {
			seenIDs[ancestor.ID] = true
		}
		upper := start
		for _, entry := range page.Entries {
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			position := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
			if !position.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID ||
				entry.ParentID == nil || *entry.ParentID != parentID || entry.Status != "ACTIVE" ||
				entry.Revision == 0 || entry.Name == "" {
				return domain.ErrInvalidFileEntryData
			}
			if entry.EntryType != "FILE" && entry.EntryType != "DIR" {
				return domain.ErrInvalidFileEntryData
			}
			if entry.EntryType == "DIR" && entry.FileSize != 0 {
				return domain.ErrInvalidFileEntryData
			}
			if upper.ID != 0 && (position.ModifiedAt.After(upper.ModifiedAt) || (position.ModifiedAt.Equal(upper.ModifiedAt) && position.ID >= upper.ID)) {
				return domain.ErrInvalidFileEntryData
			}
			seenIDs[entry.ID] = true
			entryCopy := entry
			parentCopy := *entry.ParentID
			entryCopy.ParentID = &parentCopy
			entries = append(entries, entryCopy)
			upper = position
		}
		prepared = &domain.DirectoryList{Path: *path, Entries: entries, HasMore: page.HasMore}
		return nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取目录列表快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
