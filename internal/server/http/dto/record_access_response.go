package dto

import (
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RecordAccessData 保存访问记录确认提交后的公开时间。
type RecordAccessData struct {
	// LastOpenedAt 是已记录的 UTC 微秒访问时间。
	LastOpenedAt time.Time `json:"last_opened_at"`
}

// RecordAccessResponse 关联访问记录成功数据与本次请求 ID。
type RecordAccessResponse struct {
	// Data 保存已转换的访问时间。
	Data RecordAccessData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志保持一致。
	RequestID string `json:"request_id"`
}

// NewRecordAccessData 转换服务确认提交后的访问偏好，失败返回空结果。
func NewRecordAccessData(result *domain.EntryPreference) (*RecordAccessData, error) {
	// 1. 检查已提交偏好、必要标识和访问时间。
	if result == nil || result.UserID == 0 || result.EntryID == 0 || result.LastOpenedAt == nil {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 复制并规范化访问时间。
	lastOpenedAt, valid := listResponseTime(result.LastOpenedAt)
	if !valid {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 组装访问记录成功数据。
	return &RecordAccessData{LastOpenedAt: *lastOpenedAt}, nil
}
