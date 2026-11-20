package dto

import (
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PurgeEntryProgressData 是永久清理进度的公开结果。
type PurgeEntryProgressData struct {
	// EntryID 是保留完整 uint64 精度的十进制字符串。
	EntryID string `json:"entry_id"`
	// Status 表示当前进度状态，取 PURGING 或 PURGED。
	Status string `json:"status"`
	// RemainingCount 包含尚未完成的目标自身和后代。
	RemainingCount int `json:"remaining_count"`
}

// PurgeEntryProgressResponse 将进度数据与当次请求 ID 关联。
type PurgeEntryProgressResponse struct {
	Data      PurgeEntryProgressData `json:"data"`
	RequestID string                 `json:"request_id"`
}

// NewPurgeEntryProgressData 校验并转换永久清理进度。
func NewPurgeEntryProgressData(result *domain.PurgeProgress) (*PurgeEntryProgressData, error) {
	// 1. 检查结果和目标 ID。
	if result == nil || result.EntryID == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 核对状态和剩余项数的组合。
	switch result.Status {
	case "PURGING":
		if result.RemainingCount < 1 || result.RemainingCount > domain.MaxFileEntrySubtreeNodes {
			return nil, domain.ErrInvalidFileEntryData
		}
	case "PURGED":
		if result.RemainingCount != 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
	default:
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 转换目标 ID，组装公开响应数据。
	return &PurgeEntryProgressData{
		EntryID:        strconv.FormatUint(result.EntryID, 10),
		Status:         result.Status,
		RemainingCount: result.RemainingCount,
	}, nil
}
