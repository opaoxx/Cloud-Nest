package dto

import (
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UserSessionSummary 保存客户端展示一条用户会话所需的字段。
type UserSessionSummary struct {
	// ID 是完整 uint64 会话主键的十进制字符串。
	ID string `json:"id"`
	// Device 复用设备标识、名称和平台摘要。
	Device LoginDeviceSummary `json:"device"`
	// CreatedAt 是 UTC 微秒精度的会话创建时间。
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt 是 UTC 微秒精度的会话截止时间。
	ExpiresAt time.Time `json:"expires_at"`
	// LastSeenAt 保留可空的最近使用时间，空值编码为 null。
	LastSeenAt *time.Time `json:"last_seen_at"`
	// RevokedAt 保留可空的会话撤销时间，空值编码为 null。
	RevokedAt *time.Time `json:"revoked_at"`
	// DeviceRevokedAt 保留可空的设备撤销时间，空值编码为 null。
	DeviceRevokedAt *time.Time `json:"device_revoked_at"`
	// Current 是服务按本次请求 Actor 计算的当前会话标记。
	Current bool `json:"current"`
}

// UserSessionListData 保存本页展示记录与后续页提示。
type UserSessionListData struct {
	// Items 使用已初始化的切片，空页编码为 []。
	Items []UserSessionSummary `json:"items"`
	// HasMore 明确返回本次查询的后续页提示。
	HasMore bool `json:"has_more"`
}

// UserSessionListResponse 关联会话列表数据与本次请求 ID。
type UserSessionListResponse struct {
	// Data 保存已转换为客户端格式的本页数据。
	Data UserSessionListData `json:"data"`
	// RequestID 与后续 HTTP 响应头和日志中的标识一致。
	RequestID string `json:"request_id"`
}

// NewUserSessionListData 映射已完成归属、分页及 Current 检查的服务结果。
func NewUserSessionListData(result *domain.UserSessionList) (*UserSessionListData, error) {
	// 1. 检查服务结果及列表大小。
	if result == nil || len(result.Sessions) > 100 || (result.HasMore && len(result.Sessions) == 0) {
		return nil, domain.ErrInvalidSessionData
	}
	data := &UserSessionListData{
		Items:   make([]UserSessionSummary, 0, len(result.Sessions)),
		HasMore: result.HasMore,
	}

	// 2. 逐项转换 ID、设备信息和时间字段。
	for _, session := range result.Sessions {
		if session.ID == 0 || session.UserID == 0 || session.DeviceID == 0 {
			return nil, domain.ErrInvalidSessionData
		}
		createdAt, err := sessionListTime(&session.CreatedAt)
		if err != nil {
			return nil, err
		}
		expiresAt, err := sessionListTime(&session.ExpiresAt)
		if err != nil {
			return nil, err
		}
		lastSeenAt, err := sessionListTime(session.LastSeenAt)
		if err != nil {
			return nil, err
		}
		revokedAt, err := sessionListTime(session.RevokedAt)
		if err != nil {
			return nil, err
		}
		deviceRevokedAt, err := sessionListTime(session.DeviceRevokedAt)
		if err != nil {
			return nil, err
		}
		data.Items = append(data.Items, UserSessionSummary{
			ID: strconv.FormatUint(session.ID, 10),
			Device: LoginDeviceSummary{
				ID:       strconv.FormatUint(session.DeviceID, 10),
				Name:     session.DeviceName,
				Platform: session.DevicePlatform,
			},
			CreatedAt:       *createdAt,
			ExpiresAt:       *expiresAt,
			LastSeenAt:      lastSeenAt,
			RevokedAt:       revokedAt,
			DeviceRevokedAt: deviceRevokedAt,
			Current:         session.Current,
		})
	}

	// 3. 返回列表数据与分页提示。
	return data, nil
}

// sessionListTime 将可空数据库时间复制为 UTC 微秒值。
func sessionListTime(value *time.Time) (*time.Time, error) {
	// 1. 保留空值，复制并规范化非空时间。
	if value == nil {
		return nil, nil
	}
	normalized := value.UTC().Truncate(time.Microsecond)

	// 2. 检查 UTC 年份处于数据库 DATETIME 的有效范围。
	if normalized.Year() < 1000 || normalized.Year() > 9999 {
		return nil, domain.ErrInvalidSessionData
	}
	return &normalized, nil
}
