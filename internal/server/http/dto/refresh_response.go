package dto

import (
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RefreshData 保存刷新成功时明确交付的凭证字段。
type RefreshData struct {
	// AccessToken 是后续受保护请求使用的 JWT 原文。
	AccessToken string `json:"access_token"`
	// ExpiresIn 是响应计算时刻到访问截止时间的完整剩余秒数。
	ExpiresIn int64 `json:"expires_in"`
	// RefreshToken 在原生客户端响应中交付，Web 使用后续 Cookie 交付。
	RefreshToken string `json:"refresh_token,omitempty"`
}

// RefreshResponse 关联刷新成功数据与本次请求 ID。
type RefreshResponse struct {
	// Data 保存已完成转换的刷新数据。
	Data RefreshData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志中的标识一致。
	RequestID string `json:"request_id"`
}

// NewRefreshData 映射已提交且签发成功的刷新结果。
// clientType 须经过请求及来源校验，now 由后续 HTTP 交付方提供。
func NewRefreshData(result *domain.RefreshTokenResult, clientType string, now time.Time) (*RefreshData, error) {
	// 1. 检查刷新结果、客户端类型和计算时间。
	if result == nil || result.Session.ID == 0 || result.Session.UserID == 0 || result.Session.DeviceID == 0 ||
		result.TokenGeneration < 2 {
		return nil, domain.ErrInvalidSessionData
	}
	if clientType != "web" && clientType != "native" {
		return nil, fmt.Errorf("刷新响应需要有效的客户端类型")
	}
	if result.AccessToken.Token == "" || result.RefreshToken == "" || now.IsZero() {
		return nil, fmt.Errorf("刷新响应凭证或计算时间无效")
	}

	// 2. 检查会话与访问令牌的期限。
	sessionExpiry := result.Session.ExpiresAt.UTC().Truncate(time.Microsecond)
	accessExpiry := result.AccessToken.ExpiresAt.UTC()
	if sessionExpiry.Year() < 1000 || sessionExpiry.Year() > 9999 || accessExpiry.IsZero() ||
		accessExpiry.After(sessionExpiry) {
		return nil, domain.ErrInvalidSessionData
	}
	if !sessionExpiry.After(now) {
		return nil, domain.ErrSessionExpired
	}
	if !accessExpiry.After(now) {
		return nil, domain.ErrAccessTokenExpired
	}

	// 3. 计算剩余秒数，按客户端类型组装响应数据。
	data := &RefreshData{
		AccessToken: result.AccessToken.Token,
		ExpiresIn:   int64(accessExpiry.Sub(now) / time.Second),
	}
	if clientType == "native" {
		data.RefreshToken = result.RefreshToken
	}
	return data, nil
}
