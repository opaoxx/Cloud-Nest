package dto

import (
	"strconv"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreatedDirectorySummary 保存创建时的普通目录快照，JSON 基础字段平铺。
type CreatedDirectorySummary struct {
	// DirectoryEntrySummary 复用标识、名称、类型、大小、版本、时间和状态。
	DirectoryEntrySummary
	// ColorKey 保存已确认写入的颜色，明确返回 null 或文本。
	ColorKey *string `json:"color_key"`
}

// CreateDirectoryData 保存可交付且可写入幂等记录的稳定结果。
type CreateDirectoryData struct {
	// Entry 是创建时的普通目录快照。
	Entry CreatedDirectorySummary `json:"entry"`
}

// CreateDirectoryResponse 关联稳定创建结果与当前 HTTP 请求 ID。
type CreateDirectoryResponse struct {
	// Data 保存创建成功的稳定数据。
	Data CreateDirectoryData `json:"data"`
	// RequestID 由交付层按当前请求提供。
	RequestID string `json:"request_id"`
}

// NewCreateDirectoryData 转换新目录快照和已保存颜色，供事务内结果保存及提交后交付使用。
// 调用方在外层事务确认提交后交付响应，异常输入返回 nil 和目录项数据错误。
func NewCreateDirectoryData(result *domain.FileEntry, colorKey *string) (*CreateDirectoryData, error) {
	// 1. 检查新建普通目录的基础记录及颜色。
	if result == nil || result.ID == 0 || result.UserID == 0 || result.ParentID == nil || *result.ParentID == 0 || *result.ParentID == result.ID ||
		result.Name == "" || !utf8.ValidString(result.Name) || result.EntryType != "DIR" || result.FileSize != 0 || result.Revision != 1 ||
		result.Status != "ACTIVE" || !domain.ValidDirectoryColorKey(colorKey) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 复制并规范化三个目录时间。
	createdAt, validCreated := listResponseTime(&result.CreatedAt)
	modifiedAt, validModified := listResponseTime(&result.ModifiedAt)
	updatedAt, validUpdated := listResponseTime(&result.UpdatedAt)
	if !validCreated || !validModified || !validUpdated {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 组装基础字段和独立颜色值，返回稳定响应数据。
	return &CreateDirectoryData{Entry: CreatedDirectorySummary{
		DirectoryEntrySummary: DirectoryEntrySummary{
			ID:         strconv.FormatUint(result.ID, 10),
			ParentID:   strconv.FormatUint(*result.ParentID, 10),
			Name:       result.Name,
			EntryType:  result.EntryType,
			SizeBytes:  result.FileSize,
			Revision:   strconv.FormatUint(result.Revision, 10),
			CreatedAt:  *createdAt,
			ModifiedAt: *modifiedAt,
			UpdatedAt:  *updatedAt,
			Status:     result.Status,
		},
		ColorKey: copyEntryText(colorKey),
	}}, nil
}
