package dto

import (
	"strconv"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RestoredEntrySummary 保存单项恢复的公开字段。
type RestoredEntrySummary struct {
	// ID 是恢复项 ID 的十进制字符串。
	ID string `json:"id"`
	// ParentID 是实际恢复父目录的 ID，回退时为用户真实根目录 ID。
	ParentID string `json:"parent_id"`
	// Name 是恢复后的最终展示名。
	Name string `json:"name"`
	// EntryType 为 FILE 或 DIR。
	EntryType string `json:"entry_type"`
	// Status 为 ACTIVE。
	Status string `json:"status"`
	// Revision 是恢复后版本的十进制字符串。
	Revision string `json:"revision"`
}

// RestoreEntryData 保存可用于幂等记录的稳定恢复快照。
type RestoreEntryData struct {
	Entry RestoredEntrySummary `json:"entry"`
}

// RestoreEntryResponse 关联稳定恢复快照与当前请求 ID。
type RestoreEntryResponse struct {
	Data      RestoreEntryData `json:"data"`
	RequestID string           `json:"request_id"`
}

// NewRestoreEntryData 将恢复写入结果转换为公开快照，外层确认提交后才交付。
func NewRestoreEntryData(result *domain.FileEntry) (*RestoreEntryData, error) {
	// 1. 校验目录项标识、实际父目录、名称、类型和活动状态。
	if result == nil || result.ID == 0 || result.UserID == 0 || result.ParentID == nil ||
		*result.ParentID == 0 || *result.ParentID == result.ID || result.Revision == 0 ||
		result.Name == "" || !utf8.ValidString(result.Name) || result.Status != "ACTIVE" ||
		(result.EntryType != "FILE" && result.EntryType != "DIR") || (result.EntryType == "DIR" && result.FileSize != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 把 ID 和版本转换为十进制字符串。
	entry := RestoredEntrySummary{
		ID:        strconv.FormatUint(result.ID, 10),
		ParentID:  strconv.FormatUint(*result.ParentID, 10),
		Name:      result.Name,
		EntryType: result.EntryType,
		Status:    result.Status,
		Revision:  strconv.FormatUint(result.Revision, 10),
	}

	// 3. 组装可序列化的恢复快照。
	return &RestoreEntryData{Entry: entry}, nil
}
