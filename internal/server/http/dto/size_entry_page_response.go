package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// SizeEntryPageData 保存大小可见项、大小视图面包屑及可空下一页游标。
type SizeEntryPageData struct {
	// Items 空页明确编码为 []，复用已有十字段基础摘要。
	Items []DirectoryEntrySummary `json:"items"`
	// Breadcrumbs 使用空切片表达跨目录的大小视图。
	Breadcrumbs []DirectoryBreadcrumb `json:"breadcrumbs"`
	// NextCursor 保留预算空页的续页文本，结束时编码为 null。
	NextCursor *string `json:"next_cursor"`
}

// SizeEntryPageResponse 关联大小分页数据与当前请求 ID。
type SizeEntryPageResponse struct {
	// Data 保存已转换的公开数据。
	Data SizeEntryPageData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志保持一致。
	RequestID string `json:"request_id"`
}

// NewSizeEntryPageData 转换已完成可见性及文本游标处理的大小分页结果。
func NewSizeEntryPageData(result *domain.SizeEntryPageResult) (*SizeEntryPageData, error) {
	// 1. 检查分页结果、数量和大小游标容量。
	if result == nil || len(result.Entries) > 100 {
		return nil, domain.ErrInvalidFileEntryData
	}
	if result.NextCursor != nil && (*result.NextCursor == "" || len(*result.NextCursor) > domain.MaxSizeEntryCursorLength) {
		return nil, domain.ErrInvalidFileEntryData
	}
	data := &SizeEntryPageData{
		Items:       make([]DirectoryEntrySummary, 0, len(result.Entries)),
		Breadcrumbs: make([]DirectoryBreadcrumb, 0),
	}

	// 2. 复用条目摘要转换，核对同一用户并保留结果顺序。
	var ownerID uint64
	for _, entry := range result.Entries {
		if ownerID == 0 {
			ownerID = entry.UserID
		}
		if entry.UserID != ownerID {
			return nil, domain.ErrInvalidFileEntryData
		}
		summary, err := newDirectoryEntrySummary(entry)
		if err != nil {
			return nil, err
		}
		data.Items = append(data.Items, *summary)
	}

	// 3. 复制下一页游标，交付大小分页响应数据。
	if result.NextCursor != nil {
		cursor := *result.NextCursor
		data.NextCursor = &cursor
	}
	return data, nil
}
