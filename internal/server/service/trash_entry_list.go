package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const maxTrashEntryCandidates = 1000

// TrashEntryListReader 提供同一只读快照内的候选和祖先读取。
type TrashEntryListReader interface {
	FileEntryPathReader
	ListUserTrashEntryCandidates(context.Context, uint64, int, *domain.TrashEntryPosition) (*domain.TrashEntryPage, error)
}

// TrashEntryListService 组合回收候选扫描和祖先检查。
type TrashEntryListService struct {
	// withSnapshot 同步执行读取回调，成功结束快照后返回 nil。
	withSnapshot func(context.Context, func(context.Context, TrashEntryListReader) error) error
}

// NewTrashEntryListService 绑定只读快照入口。
func NewTrashEntryListService(withSnapshot func(context.Context, func(context.Context, TrashEntryListReader) error) error) *TrashEntryListService {
	// 1. 检查并保存快照读取能力。
	if withSnapshot == nil {
		panic("回收站列表需要只读快照方法")
	}
	return &TrashEntryListService{withSnapshot: withSnapshot}
}

// List 在同一只读快照内读取可见回收项。
func (listService *TrashEntryListService) List(ctx context.Context, actor domain.Actor, pageSize int, before *domain.TrashEntryPosition) (*domain.TrashEntryList, error) {
	// 1. 核对身份、页大小和起始位置，打开只读快照。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("回收站列表需要上下文、完整认证身份和 1～100 的页大小")
	}
	position := copyOptionalValue(before)
	if position != nil {
		if !position.IsValid() {
			return nil, fmt.Errorf("回收站续读位置需要非零 ID 和数据库微秒时间")
		}
		position.DeletedAt = position.DeletedAt.UTC()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result *domain.TrashEntryList
	snapshotError := listService.withSnapshot(ctx, func(snapshotContext context.Context, reader TrashEntryListReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("回收站列表回调需要快照上下文和读取能力")
		}
		var err error
		result, err = readTrashEntryList(snapshotContext, reader, actor, pageSize, position)
		return err
	})

	// 4. 确认快照成功结束，再交付结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取回收站列表快照失败: %w", snapshotError)
	}
	if result == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return result, nil
}

func readTrashEntryList(ctx context.Context, reader TrashEntryListReader, actor domain.Actor, pageSize int, before *domain.TrashEntryPosition) (*domain.TrashEntryList, error) {
	// 2. 批量读取候选，逐条检查祖先并记录最后消费的位置。
	profile, err := NewUserProfileService(reader.FindUserProfile).Get(ctx, actor.UserID)
	if err != nil {
		return nil, err
	}
	pathReader := &trashListPathReader{
		visibleEntryPathReader: visibleEntryPathReader{profile: *profile, entries: make(map[uint64]domain.FileEntry)},
		source:                 reader,
	}
	result := &domain.TrashEntryList{Entries: make([]domain.TrashEntryListItem, 0, pageSize)}
	seenIDs := make(map[uint64]bool)
	remaining := maxTrashEntryCandidates
	for remaining > 0 {
		batchSize := min(100, remaining)
		batch, readError := reader.ListUserTrashEntryCandidates(ctx, actor.UserID, batchSize, copyOptionalValue(before))
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if readError != nil {
			return nil, fmt.Errorf("读取回收站候选批次失败: %w", readError)
		}
		if batch == nil || len(batch.Entries) > batchSize || (batch.HasMore && len(batch.Entries) != batchSize) {
			return nil, domain.ErrInvalidFileEntryData
		}
		for index, candidate := range batch.Entries {
			position := domain.TrashEntryPosition{ID: candidate.ID, DeletedAt: candidate.DeletedAt.UTC()}
			if seenIDs[candidate.ID] || (before != nil && (position.DeletedAt.After(before.DeletedAt) ||
				(position.DeletedAt.Equal(before.DeletedAt) && position.ID >= before.ID))) {
				return nil, domain.ErrInvalidFileEntryData
			}
			candidate.ParentID = copyOptionalValue(candidate.ParentID)
			candidate.DeletedAt = position.DeletedAt
			path, err := CheckTrashEntryVisibility(ctx, actor, candidate, pathReader)
			if err != nil {
				return nil, err
			}
			seenIDs[candidate.ID] = true
			before = &position
			remaining--
			if path != nil {
				result.Entries = append(result.Entries, domain.TrashEntryListItem{Entry: candidate, OriginalPath: *path})
			}

			// 3. 页满或达到扫描上限时，按剩余候选决定是否保留续读位置。
			if len(result.Entries) == pageSize {
				if index+1 < len(batch.Entries) || batch.HasMore {
					result.NextPosition = before
				}
				return result, nil
			}
		}
		if !batch.HasMore {
			return result, nil
		}
	}
	// 能走到扫描上限表示最后一批之后仍有候选。
	result.NextPosition = before
	return result, nil
}

// trashListPathReader 在同一快照内复用已读取的用户资料和祖先记录。
type trashListPathReader struct {
	visibleEntryPathReader
	source FileEntryPathReader
}

func (reader *trashListPathReader) FindUserFileEntry(ctx context.Context, userID, entryID uint64) (*domain.FileEntry, error) {
	// 1. 只在当前用户的缓存未命中时读取快照，保存独立记录。
	if _, cached := reader.entries[entryID]; !cached && userID == reader.profile.ID {
		entry, err := reader.source.FindUserFileEntry(ctx, userID, entryID)
		if err != nil || entry == nil {
			return entry, err
		}
		record := *entry
		record.ParentID = copyOptionalValue(entry.ParentID)
		reader.entries[entryID] = record
	}
	// 2. 按现有路径读取规则交付独立副本。
	return reader.visibleEntryPathReader.FindUserFileEntry(ctx, userID, entryID)
}
