package dto

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// EntrySourceDeviceSummary 保存内容发布时的来源设备展示信息。
type EntrySourceDeviceSummary struct {
	// ID 是完整设备 ID 的十进制字符串，历史记录缺失时为 null。
	ID *string `json:"id"`
	// Name 保留发布时名称快照，历史记录缺失时为 null。
	Name *string `json:"name"`
}

// EntryDetailData 保存指定文件或目录的公开详情字段。
type EntryDetailData struct {
	// ID 是目标目录项 ID 的十进制字符串。
	ID string `json:"id"`
	// ParentID 是父目录 ID 字符串，真实根目录为 null。
	ParentID *string `json:"parent_id"`
	// Name 保留目录项名称，真实根目录为空字符串。
	Name string `json:"name"`
	// Kind 为 folder 或由文件类别映射的小写类型。
	Kind string `json:"kind"`
	// SizeBytes 是 JSON 安全整数范围内的内容字节数，目录为零。
	SizeBytes uint64 `json:"size_bytes"`
	// Revision 是完整版本号的十进制字符串。
	Revision string `json:"revision"`
	// CreatedAt 是 UTC 微秒精度的创建时间。
	CreatedAt time.Time `json:"created_at"`
	// ModifiedAt 是 UTC 微秒精度的业务修改时间。
	ModifiedAt time.Time `json:"modified_at"`
	// UpdatedAt 是 UTC 微秒精度的记录更新时间。
	UpdatedAt time.Time `json:"updated_at"`
	// Status 明确交付活动状态。
	Status string `json:"status"`
	// Extension 保留可空扩展名提示，空文本与 null 分别返回。
	Extension *string `json:"extension"`
	// MIMEType 保留可空内容 MIME。
	MIMEType *string `json:"mime_type"`
	// ColorKey 保留可空文件夹颜色键。
	ColorKey *string `json:"color_key"`
	// ContentModifiedAt 是可空的 UTC 微秒内容发布时间。
	ContentModifiedAt *time.Time `json:"content_modified_at"`
	// SourceDevice 保存可用的来源设备信息，全部缺失时为 null。
	SourceDevice *EntrySourceDeviceSummary `json:"source_device"`
	// Starred 根据收藏时间是否存在生成。
	Starred bool `json:"starred"`
	// StarredAt 是可空的 UTC 微秒收藏时间。
	StarredAt *time.Time `json:"starred_at"`
	// LastOpenedAt 是可空的 UTC 微秒最近成功授权访问时间。
	LastOpenedAt *time.Time `json:"last_opened_at"`
}

// EntryDetailResponse 关联目录项详情与本次请求 ID。
type EntryDetailResponse struct {
	// Data 保存已转换的公开详情。
	Data EntryDetailData `json:"data"`
	// RequestID 与后续响应头和完成日志保持一致。
	RequestID string `json:"request_id"`
}

// NewEntryDetailData 映射已通过可见性检查的内部详情，失败返回空结果。
func NewEntryDetailData(result *domain.EntryDetail) (*EntryDetailData, error) {
	// 1. 检查详情及根到目标的路径结构。
	if result == nil || len(result.Path.Entries) == 0 || len(result.Path.Entries) > domain.MaxFileEntryDepth+1 {
		return nil, domain.ErrInvalidFileEntryData
	}
	ownerID := result.Path.Entries[0].UserID
	if ownerID == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}
	seenIDs := make(map[uint64]bool, len(result.Path.Entries))
	for index, entry := range result.Path.Entries {
		if entry.ID == 0 || entry.UserID != ownerID || entry.Revision == 0 || entry.Status != "ACTIVE" || seenIDs[entry.ID] ||
			(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if index < len(result.Path.Entries)-1 && entry.EntryType != "DIR" {
			return nil, domain.ErrInvalidFileEntryData
		}
		if index == 0 {
			if entry.ParentID != nil || entry.Name != "" || entry.EntryType != "DIR" {
				return nil, domain.ErrInvalidFileEntryData
			}
		} else if entry.ParentID == nil || *entry.ParentID != result.Path.Entries[index-1].ID || entry.Name == "" {
			return nil, domain.ErrInvalidFileEntryData
		}
		seenIDs[entry.ID] = true
	}
	target := result.Path.Entries[len(result.Path.Entries)-1]
	display, preference := result.Display, result.Preference

	// 2. 核对展示和偏好归属，映射前端文件类别。
	if display.ID != target.ID || display.UserID != ownerID || preference.EntryID != target.ID || preference.UserID != ownerID ||
		(display.SourceDeviceID != nil && *display.SourceDeviceID == 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	kind := "folder"
	if target.EntryType == "DIR" {
		if display.Category != nil {
			return nil, domain.ErrInvalidFileEntryData
		}
	} else {
		if display.Category == nil {
			return nil, domain.ErrInvalidFileEntryData
		}
		switch *display.Category {
		case "DOCUMENT", "IMAGE", "VIDEO", "AUDIO", "ARCHIVE", "OTHER":
			kind = strings.ToLower(*display.Category)
		default:
			return nil, domain.ErrInvalidFileEntryData
		}
	}

	// 3. 转换标识、大小及可空展示字段。
	if target.FileSize > maxJSONSafeInteger {
		return nil, fmt.Errorf("目录项字节数超出 JSON 安全整数范围")
	}
	data := &EntryDetailData{
		ID:        strconv.FormatUint(target.ID, 10),
		Name:      target.Name,
		Kind:      kind,
		SizeBytes: target.FileSize,
		Revision:  strconv.FormatUint(target.Revision, 10),
		Status:    target.Status,
		Extension: copyEntryText(display.Extension),
		MIMEType:  copyEntryText(display.MIMEType),
		ColorKey:  copyEntryText(display.ColorKey),
	}
	if target.ParentID != nil {
		parentID := strconv.FormatUint(*target.ParentID, 10)
		data.ParentID = &parentID
	}

	// 4. 规范化时间并生成来源设备和收藏状态。
	createdAt, validCreated := listResponseTime(&target.CreatedAt)
	modifiedAt, validModified := listResponseTime(&target.ModifiedAt)
	updatedAt, validUpdated := listResponseTime(&target.UpdatedAt)
	contentModifiedAt, validContent := listResponseTime(display.ContentModifiedAt)
	starredAt, validStarred := listResponseTime(preference.StarredAt)
	lastOpenedAt, validOpened := listResponseTime(preference.LastOpenedAt)
	if !validCreated || !validModified || !validUpdated || !validContent || !validStarred || !validOpened {
		return nil, domain.ErrInvalidFileEntryData
	}
	data.CreatedAt, data.ModifiedAt, data.UpdatedAt = *createdAt, *modifiedAt, *updatedAt
	data.ContentModifiedAt, data.StarredAt, data.LastOpenedAt = contentModifiedAt, starredAt, lastOpenedAt
	data.Starred = starredAt != nil
	if display.SourceDeviceID != nil || display.SourceDeviceName != nil {
		data.SourceDevice = &EntrySourceDeviceSummary{Name: copyEntryText(display.SourceDeviceName)}
		if display.SourceDeviceID != nil {
			deviceID := strconv.FormatUint(*display.SourceDeviceID, 10)
			data.SourceDevice.ID = &deviceID
		}
	}

	// 5. 返回详情响应数据。
	return data, nil
}

// copyEntryText 为可空展示文本复制值，保留 nil 与空字符串。
func copyEntryText(source *string) *string {
	// 1. 保留空值，存在文本时返回独立指针。
	if source == nil {
		return nil
	}
	text := *source
	return &text
}
