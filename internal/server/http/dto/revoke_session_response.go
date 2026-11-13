package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// RevokeSessionData 保存指定会话撤销确认提交后的成功数据。
type RevokeSessionData struct {
	// Revoked 为 true 表示目标已处于撤销状态，事务已确认提交。
	Revoked bool `json:"revoked"`
}

// RevokeSessionResponse 关联指定会话撤销数据与本次请求 ID。
type RevokeSessionResponse struct {
	// Data 保存已完成转换的撤销数据。
	Data RevokeSessionData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志中的标识一致。
	RequestID string `json:"request_id"`
}

// NewRevokeSessionData 映射指定会话撤销服务确认提交后的结果，异常结果返回 nil。
func NewRevokeSessionData(result *domain.RevokeSessionResult) (*RevokeSessionData, error) {
	// 1. 检查服务结果及撤销确认标记。
	if result == nil || !result.Revoked {
		return nil, domain.ErrInvalidSessionData
	}

	// 2. 复制撤销状态，生成独立的响应数据。
	return &RevokeSessionData{Revoked: result.Revoked}, nil
}
