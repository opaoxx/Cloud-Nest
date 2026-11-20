package dto

import (
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// TrashedEntrySummary 保存回收后的七项公开字段。
type TrashedEntrySummary struct {
	// ID 是目录项 ID 的十进制字符串。
	ID string `json:"id"`
	// ParentID 保留回收时的父目录 ID。
	ParentID string `json:"parent_id"`
	// Name 保留目录项的原展示名。
	Name string `json:"name"`
	// EntryType 为 FILE 或 DIR。
	EntryType string `json:"entry_type"`
	// Status 表示该快照记录的 TRASHED 状态。
	Status string `json:"status"`
	// Revision 是回收后版本的十进制字符串。
	Revision string `json:"revision"`
	// DeletedAt 是 UTC 微秒精度的回收时间。
	DeletedAt time.Time `json:"deleted_at"`
}

// TrashEntryData 保存可交付且可写入幂等记录的稳定回收结果。
type TrashEntryData struct {
	// Entry 是本次回收的目录项快照。
	Entry TrashedEntrySummary `json:"entry"`
}

// TrashEntryResponse 关联稳定回收结果与当前 HTTP 请求 ID。
type TrashEntryResponse struct {
	// Data 保存公开回收快照。
	Data TrashEntryData `json:"data"`
	// RequestID 由交付层按当前请求填写。
	RequestID string `json:"request_id"`
}

// NewTrashEntryData 把同一事务中的回收结果转换为稳定公开数据。
// 调用方确认事务提交后再交付响应。
func NewTrashEntryData(result *domain.TrashedFileEntry) (*TrashEntryData, error) {
	// 1. 核对回收记录的标识、类型和状态。
	if result == nil || result.ID == 0 || result.UserID == 0 || result.ParentID == nil ||
		*result.ParentID == 0 || *result.ParentID == result.ID || result.Revision == 0 ||
		result.Name == "" || !utf8.ValidString(result.Name) || result.Status != "TRASHED" ||
		(result.EntryType != "FILE" && result.EntryType != "DIR") || (result.EntryType == "DIR" && result.FileSize != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 将回收时间规范为 UTC 微秒精度。
	deletedAt, valid := listResponseTime(&result.DeletedAt)
	if !valid {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 编码整数标识，组装公开快照。
	return &TrashEntryData{Entry: TrashedEntrySummary{
		ID:        strconv.FormatUint(result.ID, 10),
		ParentID:  strconv.FormatUint(*result.ParentID, 10),
		Name:      result.Name,
		EntryType: result.EntryType,
		Status:    result.Status,
		Revision:  strconv.FormatUint(result.Revision, 10),
		DeletedAt: *deletedAt,
	}}, nil
}
