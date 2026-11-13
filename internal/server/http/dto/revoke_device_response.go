package dto

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// RevokeDeviceData 保存设备及关联会话撤销确认提交后的成功数据。
type RevokeDeviceData struct {
	// Revoked 为 true 表示设备及关联会话已撤销，事务已确认提交。
	Revoked bool `json:"revoked"`
}

// RevokeDeviceResponse 关联设备撤销数据与本次请求 ID。
type RevokeDeviceResponse struct {
	// Data 保存已完成转换的设备撤销数据。
	Data RevokeDeviceData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志中的标识一致。
	RequestID string `json:"request_id"`
}

// NewRevokeDeviceData 映射设备撤销服务确认提交后的结果，异常结果返回 nil。
func NewRevokeDeviceData(result *domain.RevokeDeviceResult) (*RevokeDeviceData, error) {
	// 1. 检查设备撤销结果及提交确认标记。
	if result == nil || !result.Revoked {
		return nil, domain.ErrInvalidDeviceData
	}

	// 2. 复制撤销状态，生成独立的响应数据。
	return &RevokeDeviceData{Revoked: result.Revoked}, nil
}
