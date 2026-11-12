package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// LogoutData 保存退出成功时交付的当前会话撤销状态。
type LogoutData struct {
	// Revoked 为 true 表示本次会话撤销已确认提交。
	Revoked bool `json:"revoked"`
}

// LogoutResponse 关联退出成功数据与本次请求 ID。
type LogoutResponse struct {
	// Data 保存已完成转换的退出数据。
	Data LogoutData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志中的标识一致。
	RequestID string `json:"request_id"`
}

// NewLogoutData 映射退出服务确认提交后的结果，异常结果返回 nil。
func NewLogoutData(result *domain.LogoutResult) (*LogoutData, error) {
	// 1. 检查退出结果，确认当前会话已成功撤销。
	if result == nil || !result.Revoked {
		return nil, domain.ErrInvalidSessionData
	}

	// 2. 复制撤销状态，生成独立的响应数据。
	return &LogoutData{Revoked: result.Revoked}, nil
}
