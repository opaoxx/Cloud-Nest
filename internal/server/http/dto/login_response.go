package dto

import (
	"fmt"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// LoginDeviceSummary 是本次登录使用的设备展示信息。
type LoginDeviceSummary struct {
	// ID 是设备主键的十进制字符串。
	ID string `json:"id"`
	// Name 保留已有设备或新建设备的实际名称。
	Name string `json:"name"`
	// Platform 是该设备已保存的平台。
	Platform string `json:"platform"`
}

// LoginSessionSummary 保存当前会话及刷新凭证期限。
type LoginSessionSummary struct {
	// ID 是会话主键的十进制字符串。
	ID string `json:"id"`
	// DeviceID 是会话所属设备主键的十进制字符串。
	DeviceID string `json:"device_id"`
	// ExpiresAt 是 UTC 微秒精度的会话截止时间。
	ExpiresAt time.Time `json:"expires_at"`
}

// LoginData 保存登录成功时明确交付的字段。
type LoginData struct {
	// UserProfileData 复用用户、根目录与配额字段，JSON 中平铺这些字段。
	UserProfileData
	// AccessToken 是供客户端后续请求使用的 JWT 原文。
	AccessToken string `json:"access_token"`
	// ExpiresIn 是访问令牌从响应计算时刻起剩余的完整秒数。
	ExpiresIn int64 `json:"expires_in"`
	// Device 是本次登录设备。
	Device LoginDeviceSummary `json:"device"`
	// Session 是已经提交的当前会话。
	Session LoginSessionSummary `json:"session"`
	// RefreshToken 在原生客户端响应中交付，Web 响应省略该字段。
	RefreshToken string `json:"refresh_token,omitempty"`
}

// LoginResponse 关联登录成功数据与本次请求 ID。
type LoginResponse struct {
	// Data 保存已完成转换的登录数据。
	Data LoginData `json:"data"`
	// RequestID 与响应头及完成日志中的标识一致。
	RequestID string `json:"request_id"`
}

// NewLoginData 映射已提交并签发成功的登录结果，资料须已经过服务层检查。
// clientType 须来自登录 DTO 和来源策略校验，now 由后续 HTTP 交付方提供。
func NewLoginData(result *domain.LoginTokenResult, profile *domain.UserProfile, clientType string, now time.Time) (*LoginData, error) {
	// 1. 映射用户资料，核对会话与设备归属。
	profileData, err := NewUserProfileData(profile)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Session.ID == 0 || result.Device.ID == 0 ||
		result.Session.UserID != profile.ID || result.Device.UserID != profile.ID ||
		result.Session.DeviceID != result.Device.ID {
		return nil, domain.ErrInvalidSessionData
	}
	if result.Device.RevokedAt != nil {
		return nil, domain.ErrDeviceRevoked
	}

	// 2. 检查客户端类型、凭证和有效期。
	if clientType != "web" && clientType != "native" {
		return nil, fmt.Errorf("登录响应需要有效的客户端类型")
	}
	if result.AccessToken.Token == "" || result.RefreshToken == "" || now.IsZero() {
		return nil, fmt.Errorf("登录响应凭证或计算时间无效")
	}
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

	// 3. 计算剩余秒数，组合登录响应数据。
	data := &LoginData{
		UserProfileData: *profileData,
		AccessToken:     result.AccessToken.Token,
		ExpiresIn:       int64(accessExpiry.Sub(now) / time.Second),
		Device: LoginDeviceSummary{
			ID:       strconv.FormatUint(result.Device.ID, 10),
			Name:     result.Device.Name,
			Platform: result.Device.Platform,
		},
		Session: LoginSessionSummary{
			ID:        strconv.FormatUint(result.Session.ID, 10),
			DeviceID:  strconv.FormatUint(result.Session.DeviceID, 10),
			ExpiresAt: sessionExpiry,
		},
	}
	if clientType == "native" {
		data.RefreshToken = result.RefreshToken
	}
	return data, nil
}
