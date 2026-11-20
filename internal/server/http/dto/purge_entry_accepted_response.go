package dto

import (
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PurgeEntryAcceptedData 保存首次受理永久清理时的稳定快照。
type PurgeEntryAcceptedData struct {
	// EntryID 是目标目录项 ID 的十进制字符串。
	EntryID string `json:"entry_id"`
	// Status 固定为受理时的 PURGING。
	Status string `json:"status"`
	// ProgressURL 是同一目标的进度查询路径。
	ProgressURL string `json:"progress_url"`
}

// PurgeEntryAcceptedResponse 将受理快照与当次请求 ID 关联。
type PurgeEntryAcceptedResponse struct {
	Data      PurgeEntryAcceptedData `json:"data"`
	RequestID string                 `json:"request_id"`
}

// NewPurgeEntryAcceptedData 将首次受理结果转换为公开快照。
func NewPurgeEntryAcceptedData(result *domain.FileEntry) (*PurgeEntryAcceptedData, error) {
	// 1. 核对普通目录项的标识和受理状态。
	if result == nil || result.ID == 0 || result.UserID == 0 || result.Revision == 0 ||
		result.ParentID == nil || *result.ParentID == 0 || *result.ParentID == result.ID || result.Status != "PURGING" {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 将目标 ID 转为十进制字符串。
	entryID := strconv.FormatUint(result.ID, 10)

	// 3. 用同一个 ID 生成进度地址，组装受理快照。
	return &PurgeEntryAcceptedData{
		EntryID:     entryID,
		Status:      result.Status,
		ProgressURL: "/api/v1/trash/" + entryID + "/purge",
	}, nil
}
