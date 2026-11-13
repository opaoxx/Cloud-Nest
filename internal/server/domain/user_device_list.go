package domain

// UserDeviceListItem 在设备查询记录上附加本次请求的当前设备标记。
type UserDeviceListItem struct {
	// UserDevice 保存设备事实的独立副本。
	UserDevice
	// Current 表示本条设备 ID 与已认证 Actor.DeviceID 相同。
	Current bool
}

// UserDeviceList 保存服务返回的本页设备及后续页提示。
type UserDeviceList struct {
	// Devices 按设备 ID 降序排列，空结果使用已初始化的空切片。
	Devices []UserDeviceListItem
	// HasMore 保留查询时的后续页提示。
	HasMore bool
}
