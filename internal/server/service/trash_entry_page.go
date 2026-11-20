package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// TrashEntryPageService 组合回收站游标校验、内部列表和续页签发。
type TrashEntryPageService struct {
	verifyCursor func(uint64, string) (*domain.TrashEntryPosition, error)
	listEntries  func(context.Context, domain.Actor, int, *domain.TrashEntryPosition) (*domain.TrashEntryList, error)
	signCursor   func(uint64, domain.TrashEntryPosition) (string, error)
}

// NewTrashEntryPageService 保存游标和已确认快照列表的读取方法。
func NewTrashEntryPageService(
	verifyCursor func(uint64, string) (*domain.TrashEntryPosition, error),
	listEntries func(context.Context, domain.Actor, int, *domain.TrashEntryPosition) (*domain.TrashEntryList, error),
	signCursor func(uint64, domain.TrashEntryPosition) (string, error),
) *TrashEntryPageService {
	// 1. 确认装配齐全，保存三个已有方法。
	if verifyCursor == nil || listEntries == nil || signCursor == nil {
		panic("回收站分页服务需要游标校验、列表和签发方法")
	}
	return &TrashEntryPageService{verifyCursor: verifyCursor, listEntries: listEntries, signCursor: signCursor}
}

// List 读取回收站首页或文本游标续页。
// 失败或取消时返回空结果。
func (pageService *TrashEntryPageService) List(ctx context.Context, actor domain.Actor, pageSize int, cursor string) (*domain.TrashEntryPageResult, error) {
	// 1. 核对认证身份、页大小和游标长度。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("回收站分页需要上下文、完整认证身份和 1～100 的页大小")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(cursor) > domain.MaxTrashEntryCursorLength {
		return nil, domain.ErrInvalidTrashEntryCursor
	}

	// 2. 校验非空游标，得到内部起始位置。
	var before *domain.TrashEntryPosition
	if cursor != "" {
		position, verifyError := pageService.verifyCursor(actor.UserID, cursor)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if verifyError != nil {
			return nil, fmt.Errorf("校验回收站游标失败: %w", verifyError)
		}
		if position == nil || !position.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		before = &domain.TrashEntryPosition{ID: position.ID, DeletedAt: position.DeletedAt.UTC()}
	}

	// 3. 读取内部列表，核对结果并复制条目与原父路径。
	listed, listError := pageService.listEntries(ctx, actor, pageSize, copyOptionalValue(before))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if listError != nil {
		return nil, fmt.Errorf("读取回收站分页结果失败: %w", listError)
	}
	if listed == nil || len(listed.Entries) > pageSize {
		return nil, domain.ErrInvalidFileEntryData
	}
	entries := make([]domain.TrashEntryListItem, 0, len(listed.Entries))
	seenIDs := make(map[uint64]bool, len(listed.Entries))
	previous := before
	for _, item := range listed.Entries {
		entry := item.Entry
		path := item.OriginalPath.Entries
		position := domain.TrashEntryPosition{ID: entry.ID, DeletedAt: entry.DeletedAt.UTC()}
		if !position.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID || entry.Status != "TRASHED" ||
			entry.ParentID == nil || *entry.ParentID == 0 || *entry.ParentID == entry.ID || entry.Revision == 0 ||
			len(path) == 0 || len(path) > domain.MaxFileEntryDepth || path[len(path)-1].ID != *entry.ParentID ||
			(previous != nil && !trashEntryPositionFollows(position, *previous)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		for _, ancestor := range path {
			if ancestor.ID == 0 || ancestor.UserID != actor.UserID || ancestor.Status != "ACTIVE" || ancestor.EntryType != "DIR" {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
		seenIDs[entry.ID] = true
		previous = &position
		item.Entry.ParentID = copyOptionalValue(entry.ParentID)
		item.Entry.DeletedAt = position.DeletedAt
		item.OriginalPath.Entries = copyFileEntryRecords(path)
		entries = append(entries, item)
	}
	result := &domain.TrashEntryPageResult{Entries: entries}

	// 4. 有续读位置时为当前用户签发下一页游标。
	if listed.NextPosition != nil {
		next := *listed.NextPosition
		next.DeletedAt = next.DeletedAt.UTC()
		if !next.IsValid() || (before != nil && !trashEntryPositionFollows(next, *before)) ||
			(previous != nil && trashEntryPositionFollows(*previous, next)) ||
			(len(entries) == pageSize && next != *previous) {
			return nil, domain.ErrInvalidFileEntryData
		}
		nextCursor, signError := pageService.signCursor(actor.UserID, next)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if signError != nil {
			return nil, fmt.Errorf("签发回收站下一页游标失败: %w", signError)
		}
		if nextCursor == "" || len(nextCursor) > domain.MaxTrashEntryCursorLength {
			return nil, fmt.Errorf("回收站下一页游标签发结果异常")
		}
		result.NextCursor = &nextCursor
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// trashEntryPositionFollows 判断 position 是否严格排在 previous 之后。
func trashEntryPositionFollows(position, previous domain.TrashEntryPosition) bool {
	return position.DeletedAt.Before(previous.DeletedAt) ||
		(position.DeletedAt.Equal(previous.DeletedAt) && position.ID < previous.ID)
}
