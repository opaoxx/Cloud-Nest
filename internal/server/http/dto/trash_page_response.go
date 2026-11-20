package dto

import (
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const trashRetentionPeriod = 30 * 24 * time.Hour

// TrashListItem 保存回收站的一条公开记录。
type TrashListItem struct {
	TrashedEntrySummary
	// SizeBytes 为文件字节数，目录为零。
	SizeBytes uint64 `json:"size_bytes"`
	// OriginalPath 从真实根目录排列到原父目录。
	OriginalPath []DirectoryBreadcrumb `json:"original_path"`
	// RetentionExpiresAt 是回收时间加 30 天的 UTC 时间。
	RetentionExpiresAt time.Time `json:"retention_expires_at"`
}

// TrashPageData 保存可见回收项和可空下一页游标。
type TrashPageData struct {
	// Items 在空页时编码为 []。
	Items []TrashListItem `json:"items"`
	// NextCursor 在耗尽时编码为 null，预算空页也可以携带游标。
	NextCursor *string `json:"next_cursor"`
}

// TrashPageResponse 将公开回收站分页数据与当前请求 ID 关联。
type TrashPageResponse struct {
	Data      TrashPageData `json:"data"`
	RequestID string        `json:"request_id"`
}

// NewTrashPageData 将内部回收站分页结果转换为公开数据。
func NewTrashPageData(result *domain.TrashEntryPageResult) (*TrashPageData, error) {
	// 1. 检查分页结果、条目数量和可空游标。
	if result == nil || len(result.Entries) > 100 {
		return nil, domain.ErrInvalidFileEntryData
	}
	if result.NextCursor != nil && (*result.NextCursor == "" || len(*result.NextCursor) > domain.MaxTrashEntryCursorLength ||
		!utf8.ValidString(*result.NextCursor)) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 逐项转换回收摘要、原父路径和保留截止时间。
	items := make([]TrashListItem, 0, len(result.Entries))
	var ownerID, rootID uint64
	for _, item := range result.Entries {
		converted, err := newTrashListItem(item)
		if err != nil {
			return nil, err
		}
		itemRootID := item.OriginalPath.Entries[0].ID
		if ownerID == 0 {
			ownerID, rootID = item.Entry.UserID, itemRootID
		}
		if item.Entry.UserID != ownerID || itemRootID != rootID {
			return nil, domain.ErrInvalidFileEntryData
		}
		items = append(items, converted)
	}

	// 3. 组装公开条目，返回独立的分页数据。
	return &TrashPageData{Items: items, NextCursor: copyEntryText(result.NextCursor)}, nil
}

func newTrashListItem(item domain.TrashEntryListItem) (TrashListItem, error) {
	// 1. 复用回收摘要并核对公开大小范围。
	entry := item.Entry
	snapshot, err := NewTrashEntryData(&entry)
	if err != nil {
		return TrashListItem{}, err
	}
	if entry.FileSize > maxJSONSafeInteger {
		return TrashListItem{}, domain.ErrInvalidFileEntryData
	}

	// 2. 检查原父路径并生成面包屑。
	path := item.OriginalPath.Entries
	if len(path) == 0 || len(path) > domain.MaxFileEntryDepth || path[len(path)-1].ID != *entry.ParentID {
		return TrashListItem{}, domain.ErrInvalidFileEntryData
	}
	breadcrumbs := make([]DirectoryBreadcrumb, 0, len(path))
	seenIDs := map[uint64]bool{entry.ID: true}
	for index, ancestor := range path {
		if ancestor.ID == 0 || seenIDs[ancestor.ID] || ancestor.UserID != entry.UserID ||
			ancestor.Status != "ACTIVE" || ancestor.EntryType != "DIR" || !utf8.ValidString(ancestor.Name) {
			return TrashListItem{}, domain.ErrInvalidFileEntryData
		}
		if index == 0 {
			if ancestor.ParentID != nil || ancestor.Name != "" {
				return TrashListItem{}, domain.ErrInvalidFileEntryData
			}
		} else if ancestor.ParentID == nil || *ancestor.ParentID != path[index-1].ID || ancestor.Name == "" {
			return TrashListItem{}, domain.ErrInvalidFileEntryData
		}
		seenIDs[ancestor.ID] = true
		breadcrumbs = append(breadcrumbs, DirectoryBreadcrumb{ID: strconv.FormatUint(ancestor.ID, 10), Name: ancestor.Name})
	}

	// 3. 计算保留截止时间，组装公开条目。
	retentionExpiresAt := snapshot.Entry.DeletedAt.Add(trashRetentionPeriod)
	if retentionExpiresAt.Year() > 9999 {
		return TrashListItem{}, domain.ErrInvalidFileEntryData
	}
	return TrashListItem{
		TrashedEntrySummary: snapshot.Entry,
		SizeBytes:           entry.FileSize,
		OriginalPath:        breadcrumbs,
		RetentionExpiresAt:  retentionExpiresAt,
	}, nil
}
