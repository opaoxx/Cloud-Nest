package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// NameEntryPageData 保存名称升序可见项、全局视图面包屑及可空下一页游标。
type NameEntryPageData struct {
	// Items 复用十字段基础摘要，空页明确编码为 []。
	Items []DirectoryEntrySummary `json:"items"`
	// Breadcrumbs 使用空切片表达跨目录的名称视图。
	Breadcrumbs []DirectoryBreadcrumb `json:"breadcrumbs"`
	// NextCursor 保留预算空页续页文本，结束时编码为 null。
	NextCursor *string `json:"next_cursor"`
}

// NameEntryPageResponse 关联名称分页数据与当前请求 ID。
type NameEntryPageResponse struct {
	// Data 保存已转换的公开数据。
	Data NameEntryPageData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志保持一致。
	RequestID string `json:"request_id"`
}

// NewNameEntryPageData 转换已完成可见性和文本分页处理的名称升序结果。
func NewNameEntryPageData(result *domain.NameEntryPageResult) (*NameEntryPageData, error) {
	// 1. 检查分页结果、数量和名称游标容量。
	if result == nil || len(result.Entries) > 100 {
		return nil, domain.ErrInvalidFileEntryData
	}
	if result.NextCursor != nil && (*result.NextCursor == "" || len(*result.NextCursor) > domain.MaxNameEntryCursorLength) {
		return nil, domain.ErrInvalidFileEntryData
	}
	data := &NameEntryPageData{
		Items:       make([]DirectoryEntrySummary, 0, len(result.Entries)),
		Breadcrumbs: make([]DirectoryBreadcrumb, 0),
	}

	// 2. 复用条目摘要转换，核对同一用户并保留结果顺序。
	var ownerID uint64
	for _, candidate := range result.Entries {
		entry := candidate.Entry
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

	// 3. 复制下一页游标，交付名称分页响应数据。
	if result.NextCursor != nil {
		cursor := *result.NextCursor
		data.NextCursor = &cursor
	}
	return data, nil
}
