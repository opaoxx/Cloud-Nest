package dto

import (
	"fmt"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// DirectoryEntrySummary 保存目录列表与全局列表共用的一条基础展示记录。
type DirectoryEntrySummary struct {
	// ID 是当前目录项标识的十进制字符串。
	ID string `json:"id"`
	// ParentID 是真实父目录标识的十进制字符串。
	ParentID string `json:"parent_id"`
	// Name 保留子项原始展示名称。
	Name string `json:"name"`
	// EntryType 用 FILE 或 DIR 区分基础类型。
	EntryType string `json:"entry_type"`
	// SizeBytes 是 JSON 安全整数范围内的内容字节数。
	SizeBytes uint64 `json:"size_bytes"`
	// Revision 是完整无符号版本的十进制字符串。
	Revision string `json:"revision"`
	// CreatedAt 是 UTC 微秒精度的创建时间。
	CreatedAt time.Time `json:"created_at"`
	// ModifiedAt 是 UTC 微秒精度的业务修改时间。
	ModifiedAt time.Time `json:"modified_at"`
	// UpdatedAt 是 UTC 微秒精度的记录更新时间。
	UpdatedAt time.Time `json:"updated_at"`
	// Status 明确交付活动状态。
	Status string `json:"status"`
}

// DirectoryBreadcrumb 保存一个目录的公开导航字段。
type DirectoryBreadcrumb struct {
	// ID 是目录主键的十进制字符串。
	ID string `json:"id"`
	// Name 保留真实名称，根目录为空字符串。
	Name string `json:"name"`
}

// DirectoryPageData 保存子项、面包屑和可空下一页游标。
type DirectoryPageData struct {
	// Items 空页编码为 []。
	Items []DirectoryEntrySummary `json:"items"`
	// Breadcrumbs 按根目录到当前父目录排列。
	Breadcrumbs []DirectoryBreadcrumb `json:"breadcrumbs"`
	// NextCursor 在末页和空页编码为 null。
	NextCursor *string `json:"next_cursor"`
}

// DirectoryPageResponse 关联目录页数据与本次请求 ID。
type DirectoryPageResponse struct {
	// Data 保存已转换的公开数据。
	Data DirectoryPageData `json:"data"`
	// RequestID 与后续响应头和完成日志保持一致。
	RequestID string `json:"request_id"`
}

// NewDirectoryPageData 映射已完成可见性、分页及游标处理的目录页结果。
func NewDirectoryPageData(result *domain.DirectoryPageResult) (*DirectoryPageData, error) {
	// 1. 检查分页结果、路径和可空游标。
	if result == nil || len(result.Path.Entries) == 0 ||
		len(result.Path.Entries) > domain.MaxFileEntryDepth+1 || len(result.Entries) > 100 {
		return nil, domain.ErrInvalidFileEntryData
	}
	if result.NextCursor != nil && (*result.NextCursor == "" ||
		len(*result.NextCursor) > domain.MaxDirectoryCursorLength || len(result.Entries) == 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	ownerID := result.Path.Entries[0].UserID
	if ownerID == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}
	data := &DirectoryPageData{
		Items:       make([]DirectoryEntrySummary, 0, len(result.Entries)),
		Breadcrumbs: make([]DirectoryBreadcrumb, 0, len(result.Path.Entries)),
	}

	// 2. 将目录路径转换为字符串 ID 和名称的面包屑。
	for index, entry := range result.Path.Entries {
		if entry.ID == 0 || entry.UserID != ownerID || entry.EntryType != "DIR" || entry.Status != "ACTIVE" {
			return nil, domain.ErrInvalidFileEntryData
		}
		if index == 0 {
			if entry.ParentID != nil || entry.Name != "" {
				return nil, domain.ErrInvalidFileEntryData
			}
		} else if entry.ParentID == nil || *entry.ParentID != result.Path.Entries[index-1].ID || entry.Name == "" {
			return nil, domain.ErrInvalidFileEntryData
		}
		data.Breadcrumbs = append(data.Breadcrumbs, DirectoryBreadcrumb{
			ID:   strconv.FormatUint(entry.ID, 10),
			Name: entry.Name,
		})
	}

	// 3. 转换子项标识、大小、版本和 UTC 时间。
	parentID := result.Path.Entries[len(result.Path.Entries)-1].ID
	for _, entry := range result.Entries {
		if entry.UserID != ownerID || entry.ParentID == nil || *entry.ParentID != parentID {
			return nil, domain.ErrInvalidFileEntryData
		}
		summary, err := newDirectoryEntrySummary(entry)
		if err != nil {
			return nil, err
		}
		data.Items = append(data.Items, *summary)
	}

	// 4. 复制下一页游标，返回响应数据。
	if result.NextCursor != nil {
		nextCursor := *result.NextCursor
		data.NextCursor = &nextCursor
	}
	return data, nil
}

// newDirectoryEntrySummary 复用活动普通目录项的标识、大小及 UTC 微秒转换。
func newDirectoryEntrySummary(entry domain.FileEntry) (*DirectoryEntrySummary, error) {
	// 1. 核对普通项的标识、父目录、类型及活动状态。
	if entry.ID == 0 || entry.UserID == 0 || entry.ParentID == nil || *entry.ParentID == 0 ||
		entry.Revision == 0 || entry.Name == "" || entry.Status != "ACTIVE" || (entry.EntryType != "FILE" && entry.EntryType != "DIR") {
		return nil, domain.ErrInvalidFileEntryData
	}
	if entry.EntryType == "DIR" && entry.FileSize != 0 {
		return nil, domain.ErrInvalidFileEntryData
	}
	if entry.FileSize > maxJSONSafeInteger {
		return nil, fmt.Errorf("目录项字节数超出 JSON 安全整数范围")
	}
	// 2. 复用时间转换，生成独立的公开摘要值。
	createdAt, validCreated := listResponseTime(&entry.CreatedAt)
	modifiedAt, validModified := listResponseTime(&entry.ModifiedAt)
	updatedAt, validUpdated := listResponseTime(&entry.UpdatedAt)
	if !validCreated || !validModified || !validUpdated {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &DirectoryEntrySummary{
		ID:         strconv.FormatUint(entry.ID, 10),
		ParentID:   strconv.FormatUint(*entry.ParentID, 10),
		Name:       entry.Name,
		EntryType:  entry.EntryType,
		SizeBytes:  entry.FileSize,
		Revision:   strconv.FormatUint(entry.Revision, 10),
		CreatedAt:  *createdAt,
		ModifiedAt: *modifiedAt,
		UpdatedAt:  *updatedAt,
		Status:     entry.Status,
	}, nil
}
