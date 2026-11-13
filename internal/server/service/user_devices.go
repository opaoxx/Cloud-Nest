package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UserDeviceService 读取当前用户的设备列表并标记请求所属设备。
type UserDeviceService struct {
	// listDevices 绑定 Repository.ListUserDevices，使用同一调用上下文。
	listDevices func(context.Context, uint64, uint64, int) (*domain.UserDevicePage, error)
}

// NewUserDeviceService 绑定已有设备查询，缺少依赖属于装配错误。
func NewUserDeviceService(listDevices func(context.Context, uint64, uint64, int) (*domain.UserDevicePage, error)) *UserDeviceService {
	// 1. 确认查询依赖存在，保存供每次请求使用。
	if listDevices == nil {
		panic("用户设备服务需要设备列表查询方法")
	}
	return &UserDeviceService{listDevices: listDevices}
}

// List 接收已认证 Actor，核对设备查询结果并返回带 Current 标记的副本。
// beforeDeviceID 为 0 时读取首页，pageSize 范围为 1～100。
func (deviceService *UserDeviceService) List(ctx context.Context, actor domain.Actor, beforeDeviceID uint64, pageSize int) (*domain.UserDeviceList, error) {
	// 1. 检查认证身份、分页参数和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("读取设备列表需要上下文、完整认证身份和 1～100 的页大小")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 调用设备列表查询，处理错误与取消。
	page, queryError := deviceService.listDevices(ctx, actor.UserID, beforeDeviceID, pageSize)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取设备列表失败: %w", queryError)
	}

	// 3. 核对分页、归属及顺序，复制记录并标记当前设备。
	if page == nil || len(page.Devices) > pageSize || (page.HasMore && len(page.Devices) != pageSize) {
		return nil, domain.ErrInvalidDeviceData
	}
	devices := make([]domain.UserDeviceListItem, 0, len(page.Devices))
	upperDeviceID := beforeDeviceID
	for _, device := range page.Devices {
		if device.UserID != actor.UserID || device.ID == 0 || (upperDeviceID != 0 && device.ID >= upperDeviceID) {
			return nil, domain.ErrInvalidDeviceData
		}
		upperDeviceID = device.ID
		// 两项可空时间逐项复制，返回记录拥有各自的时间对象。
		deviceCopy := device
		if device.LastSeenAt != nil {
			lastSeenAt := *device.LastSeenAt
			deviceCopy.LastSeenAt = &lastSeenAt
		}
		if device.RevokedAt != nil {
			revokedAt := *device.RevokedAt
			deviceCopy.RevokedAt = &revokedAt
		}
		devices = append(devices, domain.UserDeviceListItem{UserDevice: deviceCopy, Current: device.ID == actor.DeviceID})
	}

	// 4. 确认请求仍有效，返回独立的列表结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &domain.UserDeviceList{Devices: devices, HasMore: page.HasMore}, nil
}
