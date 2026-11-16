package dto

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RecentEntrySummary 保存最近使用文件的基础摘要和当前用户访问时间。
type RecentEntrySummary struct {
	// DirectoryEntrySummary 复用已有十字段，JSON 中保持平铺结构。
	DirectoryEntrySummary
	// LastOpenedAt 是必填的 UTC 微秒访问时间。
	LastOpenedAt time.Time `json:"last_opened_at"`
}

// RecentEntryPageData 保存最近文件、全局视图面包屑及可空下一页游标。
type RecentEntryPageData struct {
	// Items 在空页时明确编码为 []。
	Items []RecentEntrySummary `json:"items"`
	// Breadcrumbs 使用空切片表达跨目录的最近使用视图。
	Breadcrumbs []DirectoryBreadcrumb `json:"breadcrumbs"`
	// NextCursor 保留预算空页续页文本，结束时编码为 null。
	NextCursor *string `json:"next_cursor"`
}

// RecentEntryPageResponse 关联最近使用分页数据与当前请求 ID。
type RecentEntryPageResponse struct {
	// Data 保存已转换的公开数据。
	Data RecentEntryPageData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志保持一致。
	RequestID string `json:"request_id"`
}

// NewRecentEntryPageData 转换已完成可见性和文本分页处理的最近使用结果。
func NewRecentEntryPageData(result *domain.RecentEntryPageResult) (*RecentEntryPageData, error) {
	// 1. 检查分页结果、数量和游标。
	if result == nil || len(result.Entries) > 100 {
		return nil, domain.ErrInvalidFileEntryData
	}
	if result.NextCursor != nil && (*result.NextCursor == "" || len(*result.NextCursor) > domain.MaxRecentEntryCursorLength) {
		return nil, domain.ErrInvalidFileEntryData
	}
	data := &RecentEntryPageData{
		Items:       make([]RecentEntrySummary, 0, len(result.Entries)),
		Breadcrumbs: make([]DirectoryBreadcrumb, 0),
	}

	// 2. 复用文件摘要转换，核对用户范围并规范化访问时间。
	var ownerID uint64
	for _, candidate := range result.Entries {
		entry := candidate.Entry
		if ownerID == 0 {
			ownerID = entry.UserID
		}
		if entry.UserID != ownerID || entry.EntryType != "FILE" {
			return nil, domain.ErrInvalidFileEntryData
		}
		summary, err := newDirectoryEntrySummary(entry)
		if err != nil {
			return nil, err
		}
		openedAt, valid := listResponseTime(&candidate.LastOpenedAt)
		if !valid {
			return nil, domain.ErrInvalidFileEntryData
		}
		data.Items = append(data.Items, RecentEntrySummary{DirectoryEntrySummary: *summary, LastOpenedAt: *openedAt})
	}

	// 3. 复制下一页游标，交付最近使用响应数据。
	if result.NextCursor != nil {
		cursor := *result.NextCursor
		data.NextCursor = &cursor
	}
	return data, nil
}
