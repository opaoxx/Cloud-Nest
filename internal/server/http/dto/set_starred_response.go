package dto

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SetStarredData 保存收藏设置确认提交后的公开偏好。
type SetStarredData struct {
	// Starred 当且仅当收藏时间存在时为 true。
	Starred bool `json:"starred"`
	// StarredAt 是可空的 UTC 微秒收藏时间，取消收藏时编码为 null。
	StarredAt *time.Time `json:"starred_at"`
}

// SetStarredResponse 关联收藏设置结果与本次请求 ID。
type SetStarredResponse struct {
	// Data 保存已转换的收藏状态和时间。
	Data SetStarredData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志保持一致。
	RequestID string `json:"request_id"`
}

// NewSetStarredData 转换收藏服务确认提交后的偏好，失败返回空结果。
func NewSetStarredData(result *domain.EntryPreference) (*SetStarredData, error) {
	// 1. 检查已提交偏好及必要标识。
	if result == nil || result.UserID == 0 || result.EntryID == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 复制并规范化可空收藏时间。
	starredAt, valid := listResponseTime(result.StarredAt)
	if !valid {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 根据收藏时间生成状态，返回响应数据。
	return &SetStarredData{Starred: starredAt != nil, StarredAt: starredAt}, nil
}
