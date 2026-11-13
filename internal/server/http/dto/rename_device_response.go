package dto

import (
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RenamedDeviceSummary 保存设备改名成功后的展示字段。
type RenamedDeviceSummary struct {
	// LoginDeviceSummary 复用字符串设备 ID、名称和平台，JSON 中平铺字段。
	LoginDeviceSummary
	// CreatedAt 是 UTC 微秒精度的设备创建时间。
	CreatedAt time.Time `json:"created_at"`
	// LastSeenAt 是可空的 UTC 最近认证活动时间，空值编码为 null。
	LastSeenAt *time.Time `json:"last_seen_at"`
	// RevokedAt 是可空的 UTC 撤销时间，保留已撤销目标的历史事实。
	RevokedAt *time.Time `json:"revoked_at"`
}

// RenameDeviceData 保存已确认提交的设备改名展示数据。
type RenameDeviceData struct {
	// Device 保存已转换为客户端格式的设备字段。
	Device RenamedDeviceSummary `json:"device"`
}

// RenameDeviceResponse 关联设备改名结果与本次请求 ID。
type RenameDeviceResponse struct {
	// Data 保存改名成功后的设备展示数据。
	Data RenameDeviceData `json:"data"`
	// RequestID 与后续 HTTP 响应头和完成日志中的标识一致。
	RequestID string `json:"request_id"`
}

// NewRenameDeviceData 转换改名服务确认提交后的设备结果，异常结果返回 nil。
func NewRenameDeviceData(result *domain.UserDevice) (*RenameDeviceData, error) {
	// 1. 检查已提交设备结果及必要标识。
	if result == nil || result.ID == 0 || result.UserID == 0 {
		return nil, domain.ErrInvalidDeviceData
	}

	// 2. 复制并规范化设备时间。
	createdAt, valid := listResponseTime(&result.CreatedAt)
	if !valid {
		return nil, domain.ErrInvalidDeviceData
	}
	lastSeenAt, valid := listResponseTime(result.LastSeenAt)
	if !valid {
		return nil, domain.ErrInvalidDeviceData
	}
	revokedAt, valid := listResponseTime(result.RevokedAt)
	if !valid {
		return nil, domain.ErrInvalidDeviceData
	}

	// 3. 组装设备展示字段，返回响应数据。
	return &RenameDeviceData{Device: RenamedDeviceSummary{
		LoginDeviceSummary: LoginDeviceSummary{
			ID:       strconv.FormatUint(result.ID, 10),
			Name:     result.Name,
			Platform: result.Platform,
		},
		CreatedAt:  *createdAt,
		LastSeenAt: lastSeenAt,
		RevokedAt:  revokedAt,
	}}, nil
}
