package dto

import (
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UserDeviceSummary 保存客户端展示一条用户设备所需的字段。
type UserDeviceSummary struct {
	// LoginDeviceSummary 复用设备 ID、名称和平台字段，JSON 中平铺这些字段。
	LoginDeviceSummary
	// CreatedAt 是 UTC 微秒精度的设备创建时间。
	CreatedAt time.Time `json:"created_at"`
	// LastSeenAt 保留可空的最近认证活动时间，空值编码为 null。
	LastSeenAt *time.Time `json:"last_seen_at"`
	// RevokedAt 保留可空的设备撤销时间，空值编码为 null。
	RevokedAt *time.Time `json:"revoked_at"`
	// Current 是服务按本次请求 Actor.DeviceID 计算的当前设备标记。
	Current bool `json:"current"`
}

// UserDeviceListData 保存本页设备展示记录与后续页提示。
type UserDeviceListData struct {
	// Items 使用已初始化的切片，空页编码为 []。
	Items []UserDeviceSummary `json:"items"`
	// HasMore 明确返回本次查询的后续页提示。
	HasMore bool `json:"has_more"`
}

// UserDeviceListResponse 关联设备列表数据与本次请求 ID。
type UserDeviceListResponse struct {
	// Data 保存已转换为客户端格式的本页数据。
	Data UserDeviceListData `json:"data"`
	// RequestID 与后续 HTTP 响应头和日志中的标识一致。
	RequestID string `json:"request_id"`
}

// NewUserDeviceListData 映射已完成归属、分页及 Current 检查的设备服务结果。
func NewUserDeviceListData(result *domain.UserDeviceList) (*UserDeviceListData, error) {
	// 1. 检查服务结果及列表大小。
	if result == nil || len(result.Devices) > 100 || (result.HasMore && len(result.Devices) == 0) {
		return nil, domain.ErrInvalidDeviceData
	}
	data := &UserDeviceListData{
		Items:   make([]UserDeviceSummary, 0, len(result.Devices)),
		HasMore: result.HasMore,
	}

	// 2. 逐项转换设备 ID、展示信息和时间字段。
	for _, device := range result.Devices {
		if device.ID == 0 || device.UserID == 0 {
			return nil, domain.ErrInvalidDeviceData
		}
		createdAt, valid := listResponseTime(&device.CreatedAt)
		if !valid {
			return nil, domain.ErrInvalidDeviceData
		}
		lastSeenAt, valid := listResponseTime(device.LastSeenAt)
		if !valid {
			return nil, domain.ErrInvalidDeviceData
		}
		revokedAt, valid := listResponseTime(device.RevokedAt)
		if !valid {
			return nil, domain.ErrInvalidDeviceData
		}
		data.Items = append(data.Items, UserDeviceSummary{
			LoginDeviceSummary: LoginDeviceSummary{
				ID:       strconv.FormatUint(device.ID, 10),
				Name:     device.Name,
				Platform: device.Platform,
			},
			CreatedAt:  *createdAt,
			LastSeenAt: lastSeenAt,
			RevokedAt:  revokedAt,
			Current:    device.Current,
		})
	}

	// 3. 返回列表数据与分页提示。
	return data, nil
}
